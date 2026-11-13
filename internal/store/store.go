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

	"guix-pull-ready/internal/cuirass"
)

// SingleJobAbsent marks a package slot in DenseSingleJobs that has no single-job
// entry (either absent from the evaluation or promoted into JobsByBase).
const SingleJobAbsent int8 = -128

// EvalSystemKey uniquely identifies an evaluation for a target system architecture.
type EvalSystemKey struct {
	EvalID int64
	System string
}

// ChannelEvalSystemKey uniquely identifies an evaluation on a specific channel and system.
type ChannelEvalSystemKey struct {
	Channel string
	EvalID  int64
	System  string
}

// ChannelBuildKey uniquely identifies a build ID on a specific channel's Cuirass server.
type ChannelBuildKey struct {
	Channel string
	BuildID int64
}

// EvaluationRecord holds metadata for an evaluation.
type EvaluationRecord struct {
	ID        int64
	Channel   string
	Commit    string
	Checkouts map[string]string
	Timestamp time.Time
}

// PullEvalStatus holds the status of a "guix" jobset evaluation for a commit and system.
type PullEvalStatus struct {
	EvalID int64
	Ready  bool
}

// PendingBuild records a sample pending job and its Cuirass build ID.
type PendingBuild struct {
	BuildID int64
	JobName string
}

// SystemEvalData holds aggregated counts and indexed jobs for an (evaluation, system) pair.
//
// Memory layout optimization:
// A single GNU Guix evaluation contains ~35,000 package jobs per architecture.
// Storing a []cuirass.Job slice or map[string]int8 for every package across evaluations would
// allocate hundreds of megabytes of map buckets and pointers. Because ~99% of packages have only
// a single job per evaluation, DenseSingleJobs stores their status as a 1-byte int8 indexed by
// dense package ID. Only the ~200 packages with multiple versions in the same
// evaluation (such as gcc-toolchain or linux-libre) are stored in JobsByBase.
type SystemEvalData struct {
	EvalID        int64
	System        string
	TotalJobs     int
	SucceededJobs int
	PendingJobs   int
	FailedJobs    int
	PendingSample []string
	PendingBuilds []PendingBuild
	PullReady     bool
	PullEvalID    int64
	Complete      bool
	// DenseSingleJobs maps dense package IDs directly to their JobStatus (as int8),
	// or SingleJobAbsent (-128) when absent or promoted into JobsByBase.
	DenseSingleJobs []int8
	// JobsByBase stores full job slices only for packages that have multiple jobs
	// in the same evaluation (e.g. gcc-toolchain, linux-libre).
	JobsByBase map[string][]cuirass.Job
}

// Snapshot is an immutable, read-optimized view of the store state used by HTTP handlers.
type Snapshot struct {
	ServerURL             string
	ChannelServerURLs     map[string]string
	Systems               []string
	LastSyncedAt          time.Time
	Evaluations           []EvaluationRecord
	ByEvalSystem          map[EvalSystemKey]*SystemEvalData
	GuixEvalByCommit      map[string]EvaluationRecord
	GuixEvalIndexByCommit map[string]int
	ChannelEvaluations    map[string][]EvaluationRecord
	ByChannelEvalSystem   map[ChannelEvalSystemKey]*SystemEvalData
	pkgID                 map[string]uint32
}

// IsPackageTracked reports whether basePkg appears in at least one cached evaluation
// for (channel, system).
func (snap *Snapshot) IsPackageTracked(channel, system, basePkg string) bool {
	if snap == nil || basePkg == "" {
		return false
	}
	id, ok := snap.pkgID[basePkg]
	if !ok {
		return false
	}
	if channel == "" || channel == "guix" {
		for k, data := range snap.ByEvalSystem {
			if k.System != system || data == nil {
				continue
			}
			if int(id) < len(data.DenseSingleJobs) && data.DenseSingleJobs[id] != SingleJobAbsent {
				return true
			}
			if _, ok := data.JobsByBase[basePkg]; ok {
				return true
			}
		}
		return false
	}
	for k, data := range snap.ByChannelEvalSystem {
		if k.Channel != channel || k.System != system || data == nil {
			continue
		}
		if int(id) < len(data.DenseSingleJobs) && data.DenseSingleJobs[id] != SingleJobAbsent {
			return true
		}
		if _, ok := data.JobsByBase[basePkg]; ok {
			return true
		}
	}
	return false
}

// LookupSingleJob returns the JobStatus of a single-job package in data, or (0, false)
// if the package is absent or stored in data.JobsByBase as a multi-version package.
func (snap *Snapshot) LookupSingleJob(data *SystemEvalData, basePkg string) (cuirass.JobStatus, bool) {
	if snap == nil || data == nil || basePkg == "" {
		return 0, false
	}
	id, ok := snap.pkgID[basePkg]
	if !ok || int(id) >= len(data.DenseSingleJobs) {
		return 0, false
	}
	raw := data.DenseSingleJobs[id]
	if raw == SingleJobAbsent {
		return 0, false
	}
	return cuirass.JobStatus(raw), true
}

// persistedState is the on-disk representation saved atomically to the database file.
type persistedState struct {
	ServerURL          string
	ChannelServerURLs  map[string]string
	Systems            []string
	LastSyncedAt       time.Time
	Evaluations        []EvaluationRecord
	SystemData         map[EvalSystemKey]*SystemEvalData
	PullByCommit       map[string]map[string]PullEvalStatus // commit -> system -> PullEvalStatus
	CommitTimes        map[string]time.Time                 // commit -> Git commit timestamp (UTC)
	BuildNixNames      map[int64]string                     // buildID -> nixname
	ChannelEvaluations map[string][]EvaluationRecord
	ChannelSystemData  map[ChannelEvalSystemKey]*SystemEvalData
	ChannelBuildNames  map[ChannelBuildKey]string
	PackageDict        []string // dense package ID -> basePkg name
}

// Store manages persistent state and publishes lock-free in-memory snapshots.
type Store struct {
	mu           sync.RWMutex
	dbPath       string
	state        persistedState
	internPool   map[string]string
	pkgID        map[string]uint32
	frozenPkgID  map[string]uint32
	scratchFirst []int32
	snapshot     atomic.Pointer[Snapshot]
}

// Open opens or initializes a persistent store at dbPath.
// Pass dbPath == "" or ":memory:" for an in-memory-only store.
func Open(dbPath string, serverURL string, systems []string) (*Store, error) {
	s := &Store{
		dbPath:     dbPath,
		internPool: make(map[string]string, 40000),
		pkgID:      make(map[string]uint32, 40000),
		state: persistedState{
			ServerURL:          serverURL,
			ChannelServerURLs:  make(map[string]string),
			Systems:            append([]string(nil), systems...),
			SystemData:         make(map[EvalSystemKey]*SystemEvalData),
			PullByCommit:       make(map[string]map[string]PullEvalStatus),
			CommitTimes:        make(map[string]time.Time),
			BuildNixNames:      make(map[int64]string),
			ChannelEvaluations: make(map[string][]EvaluationRecord),
			ChannelSystemData:  make(map[ChannelEvalSystemKey]*SystemEvalData),
			ChannelBuildNames:  make(map[ChannelBuildKey]string),
		},
	}

	if dbPath != "" && dbPath != ":memory:" {
		if err := s.loadFromDisk(); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("loading store from %s: %w", dbPath, err)
		}
	}

	s.state.ServerURL = serverURL
	if s.state.ChannelServerURLs == nil {
		s.state.ChannelServerURLs = make(map[string]string)
	}
	s.state.ChannelServerURLs["guix"] = serverURL
	s.state.Systems = append([]string(nil), systems...)
	s.rebuildSnapshotLocked()
	return s, nil
}

// SetChannelServerURL records the upstream Cuirass base URL for an additional channel.
func (s *Store) SetChannelServerURL(channel, serverURL string) {
	if channel == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.ChannelServerURLs == nil {
		s.state.ChannelServerURLs = make(map[string]string)
	}
	s.state.ChannelServerURLs[channel] = serverURL
	s.rebuildSnapshotLocked()
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

func (s *Store) internPkgIDLocked(pkg string) (string, uint32) {
	ipkg := s.internLocked(pkg)
	if id, ok := s.pkgID[ipkg]; ok {
		return ipkg, id
	}
	id := uint32(len(s.state.PackageDict))
	s.state.PackageDict = append(s.state.PackageDict, ipkg)
	s.pkgID[ipkg] = id
	return ipkg, id
}

func clampSingleStatus(st cuirass.JobStatus) int8 {
	v := int(st)
	if v <= int(SingleJobAbsent) || v > 127 {
		return int8(cuirass.StatusFailedOther)
	}
	return int8(v)
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
	if loaded.ChannelServerURLs == nil {
		loaded.ChannelServerURLs = make(map[string]string)
	}
	if loaded.SystemData == nil {
		loaded.SystemData = make(map[EvalSystemKey]*SystemEvalData)
	}
	if loaded.PullByCommit == nil {
		loaded.PullByCommit = make(map[string]map[string]PullEvalStatus)
	}
	if loaded.CommitTimes == nil {
		loaded.CommitTimes = make(map[string]time.Time)
	}
	if loaded.BuildNixNames == nil {
		loaded.BuildNixNames = make(map[int64]string)
	}
	if loaded.ChannelEvaluations == nil {
		loaded.ChannelEvaluations = make(map[string][]EvaluationRecord)
	}
	if loaded.ChannelSystemData == nil {
		loaded.ChannelSystemData = make(map[ChannelEvalSystemKey]*SystemEvalData)
	}
	if loaded.ChannelBuildNames == nil {
		loaded.ChannelBuildNames = make(map[ChannelBuildKey]string)
	}

	for id, pkg := range loaded.PackageDict {
		ipkg := s.internLocked(pkg)
		loaded.PackageDict[id] = ipkg
		s.pkgID[ipkg] = uint32(id)
	}
	s.state.PackageDict = loaded.PackageDict

	// Re-intern package and job name strings after gob decoding to deduplicate heap strings,
	// and populate cached versions on multi-version jobs.
	for k, data := range loaded.SystemData {
		if !cuirass.SupportsGuixJobset(k.System) {
			data.PullReady = true
			data.PullEvalID = 0
		}
		if len(data.JobsByBase) > 0 {
			multi := make(map[string][]cuirass.Job, len(data.JobsByBase))
			for pkg, jobs := range data.JobsByBase {
				ipkg, _ := s.internPkgIDLocked(pkg)
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

	for chKey, data := range loaded.ChannelSystemData {
		if len(data.JobsByBase) > 0 {
			multi := make(map[string][]cuirass.Job, len(data.JobsByBase))
			for pkg, jobs := range data.JobsByBase {
				ipkg, _ := s.internPkgIDLocked(pkg)
				for i := range jobs {
					jobs[i].Name = s.internLocked(jobs[i].Name)
					bKey := ChannelBuildKey{Channel: chKey.Channel, BuildID: jobs[i].BuildID}
					if nix, ok := loaded.ChannelBuildNames[bKey]; ok && nix != "" {
						jobs[i].Version = cuirass.NixnameToVersion(ipkg, nix)
					}
				}
				multi[ipkg] = jobs
			}
			data.JobsByBase = multi
		}
	}

	for i := range loaded.Evaluations {
		if ct, ok := loaded.CommitTimes[loaded.Evaluations[i].Commit]; ok && !ct.IsZero() {
			loaded.Evaluations[i].Timestamp = ct
		}
	}
	for _, bySys := range loaded.PullByCommit {
		for sys := range bySys {
			if !cuirass.SupportsGuixJobset(sys) {
				bySys[sys] = PullEvalStatus{EvalID: 0, Ready: true}
			}
		}
	}

	loaded.PackageDict = s.state.PackageDict
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

// GetChannelBuildNixName looks up a cached build ID -> nixname entry for the given channel.
func (s *Store) GetChannelBuildNixName(channel string, buildID int64) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if channel == "" || channel == "guix" {
		nix, ok := s.state.BuildNixNames[buildID]
		return nix, ok
	}
	nix, ok := s.state.ChannelBuildNames[ChannelBuildKey{Channel: channel, BuildID: buildID}]
	return nix, ok
}

// SetChannelBuildNixNames caches build ID -> nixname mappings for the specified channel
// and updates the derived Version field on any matching multi-version jobs.
func (s *Store) SetChannelBuildNixNames(channel string, nixMap map[int64]string) {
	if len(nixMap) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if channel == "" || channel == "guix" {
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
		return
	}

	if s.state.ChannelBuildNames == nil {
		s.state.ChannelBuildNames = make(map[ChannelBuildKey]string)
	}
	for buildID, nixName := range nixMap {
		if buildID <= 0 || nixName == "" {
			continue
		}
		s.state.ChannelBuildNames[ChannelBuildKey{Channel: channel, BuildID: buildID}] = nixName
		for chKey, data := range s.state.ChannelSystemData {
			if chKey.Channel != channel {
				continue
			}
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

// HasChannelEvalSystem reports whether the store has cached job data for (channel, evalID, system).
func (s *Store) HasChannelEvalSystem(channel string, evalID int64, system string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if channel == "" || channel == "guix" {
		data, ok := s.state.SystemData[EvalSystemKey{EvalID: evalID, System: system}]
		return ok && data.TotalJobs > 0
	}
	data, ok := s.state.ChannelSystemData[ChannelEvalSystemKey{Channel: channel, EvalID: evalID, System: system}]
	return ok && data.TotalJobs > 0
}

// IsChannelEvalSystemComplete reports whether a (channel, evalID, system) tuple is already cached
// with zero pending builds (and therefore never needs to be re-polled).
func (s *Store) IsChannelEvalSystemComplete(channel string, evalID int64, system string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if channel == "" || channel == "guix" {
		data, ok := s.state.SystemData[EvalSystemKey{EvalID: evalID, System: system}]
		return ok && data.Complete
	}
	data, ok := s.state.ChannelSystemData[ChannelEvalSystemKey{Channel: channel, EvalID: evalID, System: system}]
	return ok && data.Complete
}

// GetCommitTime returns any cached Git commit timestamp for the given commit hash.
func (s *Store) GetCommitTime(commit string) (time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.state.CommitTimes[commit]
	return t, ok && !t.IsZero()
}

// SetCommitTime records the Git commit timestamp for the given commit hash
// and updates any matching evaluation records in memory.
func (s *Store) SetCommitTime(commit string, t time.Time) {
	if commit == "" || t.IsZero() {
		return
	}
	utc := t.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.CommitTimes == nil {
		s.state.CommitTimes = make(map[string]time.Time)
	}
	s.state.CommitTimes[commit] = utc
	for i := range s.state.Evaluations {
		if s.state.Evaluations[i].Commit == commit {
			s.state.Evaluations[i].Timestamp = utc
		}
	}
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

// referencedGuixCommitsLocked collects all "guix" checkout commits referenced by
// currently retained additional channel evaluations. Caller must hold s.mu.
func (s *Store) referencedGuixCommitsLocked() map[string]bool {
	refs := make(map[string]bool)
	for _, chEvals := range s.state.ChannelEvaluations {
		for _, ev := range chEvals {
			if c := ev.Checkouts["guix"]; c != "" {
				refs[c] = true
			}
		}
	}
	return refs
}

// updateGuixEvaluations replaces the ordered list of recent master evaluations (sorted newest first)
// and prunes stale evaluation records beyond maxRetained to bound memory and disk usage.
// Any Guix evaluation whose commit is still referenced by a retained additional channel
// evaluation (such as nonguix) is preserved even if it has aged beyond maxRetained.
func (s *Store) updateGuixEvaluations(evals []EvaluationRecord, maxRetained int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	refCommits := s.referencedGuixCommitsLocked()
	seenID := make(map[int64]bool, len(evals))
	merged := make([]EvaluationRecord, 0, len(evals))
	for _, ev := range evals {
		if !seenID[ev.ID] {
			seenID[ev.ID] = true
			if ct, ok := s.state.CommitTimes[ev.Commit]; ok && !ct.IsZero() {
				ev.Timestamp = ct
			}
			merged = append(merged, ev)
		}
	}
	// Preserve any previously known Guix evaluation whose commit is still referenced by an additional channel.
	for _, prev := range s.state.Evaluations {
		if !seenID[prev.ID] && refCommits[prev.Commit] {
			seenID[prev.ID] = true
			if ct, ok := s.state.CommitTimes[prev.Commit]; ok && !ct.IsZero() {
				prev.Timestamp = ct
			}
			merged = append(merged, prev)
		}
	}

	sort.Slice(merged, func(i, j int) bool {
		return merged[i].ID > merged[j].ID
	})

	if maxRetained > 0 && len(merged) > 0 {
		retained := make([]EvaluationRecord, 0, maxRetained)
		keepEval := make(map[int64]bool, len(merged))
		keepCommit := make(map[string]bool, len(merged))
		for c := range refCommits {
			keepCommit[c] = true
		}
		for idx, ev := range merged {
			if idx < maxRetained || refCommits[ev.Commit] {
				retained = append(retained, ev)
				keepEval[ev.ID] = true
				keepCommit[ev.Commit] = true
			}
		}
		merged = retained
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
		for commit := range s.state.CommitTimes {
			if !keepCommit[commit] {
				delete(s.state.CommitTimes, commit)
			}
		}
	}

	s.state.Evaluations = merged
}

// UpdateChannelEvaluations replaces the ordered list of evaluations for a channel
// (sorted newest first) and prunes stale evaluation records beyond maxRetained.
func (s *Store) UpdateChannelEvaluations(channel string, evals []EvaluationRecord, maxRetained int) {
	if channel == "" || channel == "guix" {
		s.updateGuixEvaluations(evals, maxRetained)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state.ChannelEvaluations == nil {
		s.state.ChannelEvaluations = make(map[string][]EvaluationRecord)
	}

	sorted := make([]EvaluationRecord, len(evals))
	for i, ev := range evals {
		ev.Channel = channel
		if len(ev.Checkouts) > 0 {
			co := make(map[string]string, len(ev.Checkouts))
			for k, v := range ev.Checkouts {
				co[k] = v
			}
			ev.Checkouts = co
		}
		sorted[i] = ev
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ID > sorted[j].ID
	})

	if maxRetained > 0 && len(sorted) > maxRetained {
		sorted = sorted[:maxRetained]
	}
	s.state.ChannelEvaluations[channel] = sorted

	if maxRetained > 0 && len(sorted) > 0 {
		keepEval := make(map[int64]bool, len(sorted))
		for _, ev := range sorted {
			keepEval[ev.ID] = true
		}
		for k := range s.state.ChannelSystemData {
			if k.Channel == channel && !keepEval[k.EvalID] {
				delete(s.state.ChannelSystemData, k)
			}
		}
	}
}

// UpsertChannelSystemEval indexes and stores all jobs for a single (channel, evalID, commit, system).
//
// Each job is first placed into DenseSingleJobs (1 byte per package ID, pointer-free). If a second
// job with the same base package name is encountered in the same evaluation, the package is promoted
// from DenseSingleJobs into multiJobs (JobsByBase) so version-specific queries can inspect all
// candidate builds.
func (s *Store) UpsertChannelSystemEval(channel string, evalID int64, commit string, system string, jobs []cuirass.Job) {
	if channel == "" {
		channel = "guix"
	}
	isGuix := channel == "guix"
	suffix := "." + system
	var succeeded, pending, failed int
	var pendingSample []string
	var pendingBuilds []PendingBuild

	s.mu.Lock()
	defer s.mu.Unlock()

	lookupNixName := func(buildID int64) (string, bool) {
		if isGuix {
			nix, ok := s.state.BuildNixNames[buildID]
			return nix, ok
		}
		nix, ok := s.state.ChannelBuildNames[ChannelBuildKey{Channel: channel, BuildID: buildID}]
		return nix, ok
	}

	denseStatuses := make([]int8, 0, len(jobs))
	s.scratchFirst = s.scratchFirst[:0]
	multiJobs := make(map[string][]cuirass.Job)

	for jobIdx, j := range jobs {
		if !strings.HasSuffix(j.Name, suffix) {
			continue
		}
		switch {
		case j.Status.IsSucceeded():
			succeeded++
		case j.Status.IsPending():
			pending++
			if len(pendingSample) < 5 {
				ijName := s.internLocked(j.Name)
				pendingSample = append(pendingSample, ijName)
				pendingBuilds = append(pendingBuilds, PendingBuild{
					BuildID: j.BuildID,
					JobName: ijName,
				})
			}
		case j.Status.IsFailed():
			failed++
		}

		base := BasePackageFromJobName(j.Name, system)
		if base == "" {
			continue
		}
		ibase, pkgID := s.internPkgIDLocked(base)
		id := int(pkgID)
		for len(denseStatuses) <= id {
			denseStatuses = append(denseStatuses, SingleJobAbsent)
		}
		for len(s.scratchFirst) <= id {
			s.scratchFirst = append(s.scratchFirst, -1)
		}

		prevIdx := s.scratchFirst[id]
		if prevIdx == -2 {
			j.Name = s.internLocked(j.Name)
			if nix, ok := lookupNixName(j.BuildID); ok && nix != "" {
				j.Version = cuirass.NixnameToVersion(ibase, nix)
			}
			multiJobs[ibase] = append(multiJobs[ibase], j)
		} else if prevIdx >= 0 {
			// Promote from single-job to multi-job package.
			first := jobs[prevIdx]
			first.Name = s.internLocked(first.Name)
			if nix, ok := lookupNixName(first.BuildID); ok && nix != "" {
				first.Version = cuirass.NixnameToVersion(ibase, nix)
			}
			denseStatuses[id] = SingleJobAbsent
			s.scratchFirst[id] = -2

			j.Name = s.internLocked(j.Name)
			if nix, ok := lookupNixName(j.BuildID); ok && nix != "" {
				j.Version = cuirass.NixnameToVersion(ibase, nix)
			}
			multiJobs[ibase] = []cuirass.Job{first, j}
		} else {
			denseStatuses[id] = clampSingleStatus(j.Status)
			s.scratchFirst[id] = int32(jobIdx)
		}
	}

	var pullReady bool
	var pullEvalID int64
	if isGuix {
		if !cuirass.SupportsGuixJobset(system) {
			pullReady = true
		} else if bySys, ok := s.state.PullByCommit[commit]; ok {
			if st, ok := bySys[system]; ok {
				pullReady = st.Ready
				pullEvalID = st.EvalID
			}
		}
	}

	total := succeeded + pending + failed
	evalData := &SystemEvalData{
		EvalID:          evalID,
		System:          system,
		TotalJobs:       total,
		SucceededJobs:   succeeded,
		PendingJobs:     pending,
		FailedJobs:      failed,
		PendingSample:   pendingSample,
		PendingBuilds:   pendingBuilds,
		PullReady:       pullReady,
		PullEvalID:      pullEvalID,
		Complete:        total > 0 && pending == 0,
		DenseSingleJobs: denseStatuses,
		JobsByBase:      multiJobs,
	}

	if isGuix {
		s.state.SystemData[EvalSystemKey{EvalID: evalID, System: system}] = evalData
	} else {
		if s.state.ChannelSystemData == nil {
			s.state.ChannelSystemData = make(map[ChannelEvalSystemKey]*SystemEvalData)
		}
		s.state.ChannelSystemData[ChannelEvalSystemKey{Channel: channel, EvalID: evalID, System: system}] = evalData
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
// DenseSingleJobs slices are never mutated in place (UpsertChannelSystemEval allocates a fresh
// slice on replacement), so their slice headers can be shared directly with the Snapshot.
// JobsByBase slices can have their Version fields updated in place by SetChannelBuildNixNames,
// so those ~200 multi-version package slices are deep-copied to keep the Snapshot race-free.
// Caller must hold s.mu.Lock().
func (s *Store) rebuildSnapshotLocked() {
	if len(s.frozenPkgID) != len(s.pkgID) {
		frozen := make(map[string]uint32, len(s.pkgID))
		for pkg, id := range s.pkgID {
			frozen[pkg] = id
		}
		s.frozenPkgID = frozen
	}

	evals := make([]EvaluationRecord, len(s.state.Evaluations))
	guixByCommit := make(map[string]EvaluationRecord, len(s.state.Evaluations))
	guixIdxByCommit := make(map[string]int, len(s.state.Evaluations))
	for i, ev := range s.state.Evaluations {
		if len(ev.Checkouts) > 0 {
			co := make(map[string]string, len(ev.Checkouts))
			for k, v := range ev.Checkouts {
				co[k] = v
			}
			ev.Checkouts = co
		}
		evals[i] = ev
		if ev.Commit != "" {
			if _, exists := guixByCommit[ev.Commit]; !exists {
				guixByCommit[ev.Commit] = ev
				guixIdxByCommit[ev.Commit] = i
			}
		}
	}

	byEvalSys := make(map[EvalSystemKey]*SystemEvalData, len(s.state.SystemData))
	for k, v := range s.state.SystemData {
		copied := *v
		copied.DenseSingleJobs = v.DenseSingleJobs

		copiedJobsByBase := make(map[string][]cuirass.Job, len(v.JobsByBase))
		for basePkg, jobs := range v.JobsByBase {
			copiedJobs := make([]cuirass.Job, len(jobs))
			copy(copiedJobs, jobs)
			copiedJobsByBase[basePkg] = copiedJobs
		}
		copied.JobsByBase = copiedJobsByBase
		byEvalSys[k] = &copied
	}

	chEvals := make(map[string][]EvaluationRecord, len(s.state.ChannelEvaluations))
	for ch, list := range s.state.ChannelEvaluations {
		copiedList := make([]EvaluationRecord, len(list))
		for i, ev := range list {
			if len(ev.Checkouts) > 0 {
				co := make(map[string]string, len(ev.Checkouts))
				for k, v := range ev.Checkouts {
					co[k] = v
				}
				ev.Checkouts = co
			}
			copiedList[i] = ev
		}
		chEvals[ch] = copiedList
	}

	byChEvalSys := make(map[ChannelEvalSystemKey]*SystemEvalData, len(s.state.ChannelSystemData))
	for k, v := range s.state.ChannelSystemData {
		copied := *v
		copied.DenseSingleJobs = v.DenseSingleJobs

		copiedJobsByBase := make(map[string][]cuirass.Job, len(v.JobsByBase))
		for basePkg, jobs := range v.JobsByBase {
			copiedJobs := make([]cuirass.Job, len(jobs))
			copy(copiedJobs, jobs)
			copiedJobsByBase[basePkg] = copiedJobs
		}
		copied.JobsByBase = copiedJobsByBase
		byChEvalSys[k] = &copied
	}

	chURLs := make(map[string]string, len(s.state.ChannelServerURLs)+1)
	chURLs["guix"] = s.state.ServerURL
	for ch, u := range s.state.ChannelServerURLs {
		chURLs[ch] = u
	}

	snap := &Snapshot{
		ServerURL:             s.state.ServerURL,
		ChannelServerURLs:     chURLs,
		Systems:               append([]string(nil), s.state.Systems...),
		LastSyncedAt:          s.state.LastSyncedAt,
		Evaluations:           evals,
		ByEvalSystem:          byEvalSys,
		GuixEvalByCommit:      guixByCommit,
		GuixEvalIndexByCommit: guixIdxByCommit,
		ChannelEvaluations:    chEvals,
		ByChannelEvalSystem:   byChEvalSys,
		pkgID:                 s.frozenPkgID,
	}
	s.snapshot.Store(snap)
}
