// Package store manages persistent evaluation state on disk and provides
// lock-free in-memory snapshots for HTTP request handlers.
//
// State is persisted as a gob-encoded file using an atomic write-and-rename
// pattern (.tmp -> fsync -> rename) so a crash or power loss never corrupts
// the database file. Readers access an immutable Snapshot via an atomic pointer
// without contending with the background poller.
package store

import (
	"bufio"
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"guix-evaluated/internal/cuirass"
)

// EvalSystemKey uniquely identifies an evaluation for a target system architecture.
type EvalSystemKey struct {
	EvalID int64
	System string
}

// EvaluationRecord holds metadata for a master evaluation.
type EvaluationRecord struct {
	ID        int64
	Commit    string
	Timestamp time.Time
}

// PullEvalStatus holds the status of a "guix" jobset evaluation for a commit and system.
type PullEvalStatus struct {
	EvalID int64
	Ready  bool
}

// SystemEvalData holds aggregated counts and indexed jobs for an (evaluation, system) pair.
//
// Memory layout optimization:
// A single GNU Guix evaluation contains ~35,000 package jobs per architecture.
// Storing a []cuirass.Job slice for every package across 40 evaluations would allocate
// over 1.4 million slice headers and structs. Because ~99% of packages have only a single
// job per evaluation, SingleJobs stores their status directly as a 1-byte int8 keyed by
// an interned package name. Only the ~200 packages with multiple versions in the same
// evaluation (such as gcc-toolchain or linux-libre) are stored in JobsByBase.
type SystemEvalData struct {
	EvalID        int64
	System        string
	TotalJobs     int
	SucceededJobs int
	PendingJobs   int
	FailedJobs    int
	PendingSample []string
	PullReady     bool
	PullEvalID    int64
	Complete      bool
	// SingleJobs maps single-job base package names directly to their JobStatus (as int8).
	SingleJobs map[string]int8
	// JobsByBase stores full job slices only for packages that have multiple jobs
	// in the same evaluation (e.g. gcc-toolchain, linux-libre).
	JobsByBase map[string][]cuirass.Job
}

// Snapshot is an immutable, read-optimized view of the store state used by HTTP handlers.
type Snapshot struct {
	ServerURL           string
	Systems             []string
	LastSyncedAt        time.Time
	Evaluations         []EvaluationRecord
	ByEvalSystem        map[EvalSystemKey]*SystemEvalData
	TrackedBasePackages map[string]map[string]bool // system -> basePkg -> tracked
}

// persistedState is the on-disk representation saved atomically to the database file.
type persistedState struct {
	ServerURL     string
	Systems       []string
	LastSyncedAt  time.Time
	Evaluations   []EvaluationRecord
	SystemData    map[EvalSystemKey]*SystemEvalData
	PullByCommit  map[string]map[string]PullEvalStatus // commit -> system -> PullEvalStatus
	BuildNixNames map[int64]string                     // buildID -> nixname
}

// Store manages persistent state and publishes lock-free in-memory snapshots.
type Store struct {
	mu         sync.RWMutex
	dbPath     string
	state      persistedState
	internPool map[string]string
	snapshot   atomic.Pointer[Snapshot]
}

// Open opens or initializes a persistent store at dbPath.
// Pass dbPath == "" or ":memory:" for an in-memory-only store.
func Open(dbPath string, serverURL string, systems []string) (*Store, error) {
	s := &Store{
		dbPath:     dbPath,
		internPool: make(map[string]string, 40000),
		state: persistedState{
			ServerURL:     serverURL,
			Systems:       append([]string(nil), systems...),
			SystemData:    make(map[EvalSystemKey]*SystemEvalData),
			PullByCommit:  make(map[string]map[string]PullEvalStatus),
			BuildNixNames: make(map[int64]string),
		},
	}

	if dbPath != "" && dbPath != ":memory:" {
		if err := s.loadFromDisk(); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("loading store from %s: %w", dbPath, err)
		}
	}

	s.state.ServerURL = serverURL
	s.state.Systems = append([]string(nil), systems...)
	s.rebuildSnapshotLocked()
	return s, nil
}

// internLocked deduplicates repeated package and job names across evaluations
// so all evaluation maps share a single underlying string allocation per package name.
// Caller must hold s.mu.Lock().
func (s *Store) internLocked(str string) string {
	if existing, ok := s.internPool[str]; ok {
		return existing
	}
	s.internPool[str] = str
	return str
}

func (s *Store) loadFromDisk() error {
	f, err := os.Open(s.dbPath)
	if err != nil {
		return err
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 256*1024)
	var loaded persistedState
	if err := gob.NewDecoder(br).Decode(&loaded); err != nil {
		return err
	}
	if loaded.SystemData == nil {
		loaded.SystemData = make(map[EvalSystemKey]*SystemEvalData)
	}
	if loaded.PullByCommit == nil {
		loaded.PullByCommit = make(map[string]map[string]PullEvalStatus)
	}
	if loaded.BuildNixNames == nil {
		loaded.BuildNixNames = make(map[int64]string)
	}

	// Re-intern package and job name strings after gob decoding to deduplicate heap strings,
	// and populate cached versions on multi-version jobs.
	for _, data := range loaded.SystemData {
		if len(data.SingleJobs) > 0 {
			internedSingle := make(map[string]int8, len(data.SingleJobs))
			for pkg, st := range data.SingleJobs {
				internedSingle[s.internLocked(pkg)] = st
			}
			data.SingleJobs = internedSingle
		}
		if len(data.JobsByBase) > 0 {
			multi := make(map[string][]cuirass.Job, len(data.JobsByBase))
			for pkg, jobs := range data.JobsByBase {
				ipkg := s.internLocked(pkg)
				for i := range jobs {
					jobs[i].Name = s.internLocked(jobs[i].Name)
					if nix, ok := loaded.BuildNixNames[jobs[i].BuildID]; ok && nix != "" {
						jobs[i].Version = cuirass.NixnameToVersion(ipkg, nix)
					}
				}
				multi[ipkg] = jobs
			}
			data.JobsByBase = multi
		}
	}

	s.state = loaded
	return nil
}

// PublishSnapshot updates the in-memory Snapshot without writing to disk.
// Used during incremental poll cycles so HTTP clients see newly fetched evaluations immediately.
func (s *Store) PublishSnapshot() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rebuildSnapshotLocked()
}

// Save persists the current state atomically to disk and publishes a fresh Snapshot.
// It writes to a temporary file (<dbPath>.tmp), flushes buffers, fsyncs the file descriptor,
// and atomically renames it over dbPath.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.rebuildSnapshotLocked()
	if s.dbPath == "" || s.dbPath == ":memory:" {
		return nil
	}

	dir := filepath.Dir(s.dbPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmpPath := s.dbPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}

	bw := bufio.NewWriterSize(f, 256*1024)
	encErr := gob.NewEncoder(bw).Encode(&s.state)
	flushErr := bw.Flush()
	syncErr := f.Sync()
	closeFileErr := f.Close()

	if encErr != nil {
		_ = os.Remove(tmpPath)
		return encErr
	}
	if flushErr != nil {
		_ = os.Remove(tmpPath)
		return flushErr
	}
	if syncErr != nil {
		_ = os.Remove(tmpPath)
		return syncErr
	}
	if closeFileErr != nil {
		_ = os.Remove(tmpPath)
		return closeFileErr
	}

	return os.Rename(tmpPath, s.dbPath)
}

// Snapshot returns the latest immutable in-memory snapshot for lock-free reading.
func (s *Store) Snapshot() *Snapshot {
	return s.snapshot.Load()
}

// GetBuildNixName looks up a cached build ID -> nixname entry.
func (s *Store) GetBuildNixName(buildID int64) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	nix, ok := s.state.BuildNixNames[buildID]
	return nix, ok
}

// SetBuildNixNames caches build ID -> nixname mappings and updates the derived Version
// field on any matching multi-version jobs in SystemData.
func (s *Store) SetBuildNixNames(nixMap map[int64]string) {
	if len(nixMap) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for buildID, nixName := range nixMap {
		if buildID <= 0 || nixName == "" {
			continue
		}
		s.state.BuildNixNames[buildID] = nixName
		for _, data := range s.state.SystemData {
			for basePkg, jobs := range data.JobsByBase {
				for i := range jobs {
					if jobs[i].BuildID == buildID {
						jobs[i].Version = cuirass.NixnameToVersion(basePkg, nixName)
					}
				}
			}
		}
	}
}

// HasEvalSystem reports whether the store has cached job data for (evalID, system).
func (s *Store) HasEvalSystem(evalID int64, system string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.state.SystemData[EvalSystemKey{EvalID: evalID, System: system}]
	return ok && data.TotalJobs > 0
}

// IsEvalSystemComplete reports whether an (evalID, system) pair is already cached
// with zero pending builds (and therefore never needs to be re-polled).
func (s *Store) IsEvalSystemComplete(evalID int64, system string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.state.SystemData[EvalSystemKey{EvalID: evalID, System: system}]
	return ok && data.Complete
}

// GetPullStatus returns any cached "guix" jobset pull status for (commit, system).
func (s *Store) GetPullStatus(commit, system string) (PullEvalStatus, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	bySys, ok := s.state.PullByCommit[commit]
	if !ok {
		return PullEvalStatus{}, false
	}
	st, ok := bySys[system]
	return st, ok
}

// SetPullStatus records the "guix" jobset pull status for (commit, system).
func (s *Store) SetPullStatus(commit, system string, evalID int64, ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bySys, ok := s.state.PullByCommit[commit]
	if !ok {
		bySys = make(map[string]PullEvalStatus)
		s.state.PullByCommit[commit] = bySys
	}
	bySys[system] = PullEvalStatus{EvalID: evalID, Ready: ready}
	for _, ev := range s.state.Evaluations {
		if ev.Commit == commit {
			if data, exists := s.state.SystemData[EvalSystemKey{EvalID: ev.ID, System: system}]; exists {
				data.PullEvalID = evalID
				data.PullReady = ready
			}
		}
	}
}

// UpdateEvaluations replaces the ordered list of recent master evaluations (sorted newest first)
// and prunes stale evaluation records beyond maxRetained to bound memory and disk usage.
func (s *Store) UpdateEvaluations(evals []EvaluationRecord, maxRetained int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sorted := make([]EvaluationRecord, len(evals))
	copy(sorted, evals)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ID > sorted[j].ID
	})
	s.state.Evaluations = sorted

	if maxRetained > 0 && len(sorted) > 0 {
		keepEval := make(map[int64]bool, len(sorted))
		keepCommit := make(map[string]bool, len(sorted))
		for idx, ev := range sorted {
			if idx < maxRetained {
				keepEval[ev.ID] = true
				keepCommit[ev.Commit] = true
			}
		}
		for k := range s.state.SystemData {
			if !keepEval[k.EvalID] {
				delete(s.state.SystemData, k)
			}
		}
		for commit := range s.state.PullByCommit {
			if !keepCommit[commit] {
				delete(s.state.PullByCommit, commit)
			}
		}
	}
}

// UpsertSystemEval indexes and stores all jobs for a single (evalID, commit, system).
//
// Each job is first placed into singleJobs (1 byte per package). If a second job with the
// same base package name is encountered in the same evaluation, the package is promoted
// from singleJobs into multiJobs (JobsByBase) so version-specific queries can inspect all
// candidate builds.
func (s *Store) UpsertSystemEval(evalID int64, commit string, system string, jobs []cuirass.Job) {
	suffix := "." + system
	var succeeded, pending, failed int
	var pendingSample []string

	s.mu.Lock()
	defer s.mu.Unlock()

	singleJobs := make(map[string]int8, len(jobs))
	multiJobs := make(map[string][]cuirass.Job)
	firstJob := make(map[string]cuirass.Job)

	for _, j := range jobs {
		if !strings.HasSuffix(j.Name, suffix) {
			continue
		}
		switch {
		case j.Status.IsSucceeded():
			succeeded++
		case j.Status.IsPending():
			pending++
			if len(pendingSample) < 5 {
				pendingSample = append(pendingSample, s.internLocked(j.Name))
			}
		case j.Status.IsFailed():
			failed++
		}

		base := BasePackageFromJobName(j.Name, system)
		if base == "" {
			continue
		}
		ibase := s.internLocked(base)

		if existingList, isMulti := multiJobs[ibase]; isMulti {
			j.Name = s.internLocked(j.Name)
			if nix, ok := s.state.BuildNixNames[j.BuildID]; ok && nix != "" {
				j.Version = cuirass.NixnameToVersion(ibase, nix)
			}
			multiJobs[ibase] = append(existingList, j)
		} else if prevStatus, hasSingle := singleJobs[ibase]; hasSingle {
			// Promote from single-job to multi-job package.
			delete(singleJobs, ibase)
			first := firstJob[ibase]
			delete(firstJob, ibase)
			first.Name = s.internLocked(first.Name)
			first.Status = cuirass.JobStatus(prevStatus)
			if nix, ok := s.state.BuildNixNames[first.BuildID]; ok && nix != "" {
				first.Version = cuirass.NixnameToVersion(ibase, nix)
			}
			j.Name = s.internLocked(j.Name)
			if nix, ok := s.state.BuildNixNames[j.BuildID]; ok && nix != "" {
				j.Version = cuirass.NixnameToVersion(ibase, nix)
			}
			multiJobs[ibase] = []cuirass.Job{first, j}
		} else {
			singleJobs[ibase] = int8(j.Status)
			if j.Version != "" || j.BuildID > 0 {
				firstJob[ibase] = j
			}
		}
	}
	firstJob = nil // allow GC

	var pullReady bool
	var pullEvalID int64
	if bySys, ok := s.state.PullByCommit[commit]; ok {
		if st, ok := bySys[system]; ok {
			pullReady = st.Ready
			pullEvalID = st.EvalID
		}
	}

	total := succeeded + pending + failed
	s.state.SystemData[EvalSystemKey{EvalID: evalID, System: system}] = &SystemEvalData{
		EvalID:        evalID,
		System:        system,
		TotalJobs:     total,
		SucceededJobs: succeeded,
		PendingJobs:   pending,
		FailedJobs:    failed,
		PendingSample: pendingSample,
		PullReady:     pullReady,
		PullEvalID:    pullEvalID,
		Complete:      total > 0 && pending == 0,
		SingleJobs:    singleJobs,
		JobsByBase:    multiJobs,
	}
	s.state.LastSyncedAt = time.Now().UTC()
}

// BasePackageFromJobName extracts the base package name from a Cuirass job name
// by stripping the ".<system>" suffix and optional "base." prefix.
// For example:
//
//	"emacs.x86_64-linux"      -> "emacs"
//	"base.gcc.x86_64-linux"   -> "gcc"
func BasePackageFromJobName(jobName, system string) string {
	suffix := "." + system
	if !strings.HasSuffix(jobName, suffix) {
		return ""
	}
	trimmed := strings.TrimSuffix(jobName, suffix)
	trimmed = strings.TrimPrefix(trimmed, "base.")
	return trimmed
}

// rebuildSnapshotLocked constructs a new immutable Snapshot and stores it atomically.
// SingleJobs maps are never mutated in place (UpsertSystemEval allocates a fresh map on
// replacement), so their map references can be shared directly with the Snapshot.
// JobsByBase slices can have their Version fields updated in place by SetBuildNixNames,
// so those ~200 multi-version package slices are deep-copied to keep the Snapshot race-free.
// Caller must hold s.mu.Lock().
func (s *Store) rebuildSnapshotLocked() {
	evals := make([]EvaluationRecord, len(s.state.Evaluations))
	copy(evals, s.state.Evaluations)

	byEvalSys := make(map[EvalSystemKey]*SystemEvalData, len(s.state.SystemData))
	tracked := make(map[string]map[string]bool, len(s.state.Systems))
	for _, sys := range s.state.Systems {
		tracked[sys] = make(map[string]bool, 35000)
	}

	for k, v := range s.state.SystemData {
		copied := *v
		copied.SingleJobs = v.SingleJobs

		copiedJobsByBase := make(map[string][]cuirass.Job, len(v.JobsByBase))
		for basePkg, jobs := range v.JobsByBase {
			copiedJobs := make([]cuirass.Job, len(jobs))
			copy(copiedJobs, jobs)
			copiedJobsByBase[basePkg] = copiedJobs
		}
		copied.JobsByBase = copiedJobsByBase
		byEvalSys[k] = &copied

		sysMap, ok := tracked[k.System]
		if !ok {
			sysMap = make(map[string]bool, 35000)
			tracked[k.System] = sysMap
		}
		for basePkg := range v.SingleJobs {
			sysMap[basePkg] = true
		}
		for basePkg := range v.JobsByBase {
			sysMap[basePkg] = true
		}
	}

	snap := &Snapshot{
		ServerURL:           s.state.ServerURL,
		Systems:             append([]string(nil), s.state.Systems...),
		LastSyncedAt:        s.state.LastSyncedAt,
		Evaluations:         evals,
		ByEvalSystem:        byEvalSys,
		TrackedBasePackages: tracked,
	}
	s.snapshot.Store(snap)
}
