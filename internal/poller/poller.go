// Package poller synchronizes evaluation and job states from an upstream
// Cuirass server in the background and lazily resolves build version strings
// for multi-version packages.
package poller

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"guix-evaluated/internal/cuirass"
	"guix-evaluated/internal/store"
)

// Config holds configuration for the background Cuirass poller.
type Config struct {
	Systems          []string
	Limit            int
	PollInterval     time.Duration
	MaxActiveRefresh int
	Logger           *slog.Logger
}

// Poller periodically synchronizes evaluations and job statuses from Cuirass into the local Store.
type Poller struct {
	client *cuirass.Client
	store  *store.Store
	cfg    Config

	mu             sync.RWMutex
	lastPollErr    error
	lastPollEnd    time.Time
	syncing        bool
	refreshedCycle map[store.EvalSystemKey]uint64
	cycleCounter   uint64
}

// New creates a new background Poller.
func New(client *cuirass.Client, st *store.Store, cfg Config) *Poller {
	if len(cfg.Systems) == 0 {
		cfg.Systems = []string{"x86_64-linux"}
	}
	if cfg.Limit <= 0 {
		cfg.Limit = 40
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 10 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Poller{
		client:         client,
		store:          st,
		cfg:            cfg,
		refreshedCycle: make(map[store.EvalSystemKey]uint64),
	}
}

// Status returns the current health status of the poller.
func (p *Poller) Status() (syncing bool, lastEnd time.Time, lastErr error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.syncing, p.lastPollEnd, p.lastPollErr
}

// Run starts the periodic polling loop until ctx is canceled.
func (p *Poller) Run(ctx context.Context) {
	p.SyncOnce(ctx)

	ticker := time.NewTicker(p.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.SyncOnce(ctx)
		}
	}
}

// SyncOnce performs a single incremental synchronization pass against Cuirass.
// Concurrent calls while a sync is already in progress return immediately.
func (p *Poller) SyncOnce(ctx context.Context) error {
	p.mu.Lock()
	if p.syncing {
		p.mu.Unlock()
		return nil
	}
	p.syncing = true
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.syncing = false
		p.lastPollEnd = time.Now().UTC()
		p.mu.Unlock()
	}()

	err := p.syncEvaluations(ctx)
	p.mu.Lock()
	p.lastPollErr = err
	p.mu.Unlock()
	return err
}

// syncEvaluations performs the core incremental poll cycle:
//  1. Fetches the newest "master" evaluations (which build the full package collection).
//  2. Fetches recent "guix" jobset evaluations (which build the Guix package/modules
//     themselves) and correlates them by commit hash to determine whether `guix pull`
//     has prebuilt substitutes available.
//  3. For each target system, skips any evaluation already marked Complete (0 pending builds).
//  4. Fetches all uncached evaluations plus incomplete evaluations (using fair round-robin
//     ordering if MaxActiveRefresh > 0), publishing an updated in-memory snapshot after
//     each evaluation so HTTP clients can query partial progress immediately, and writing
//     the final state to disk once at the end of the cycle.
func (p *Poller) syncEvaluations(ctx context.Context) error {
	log := p.cfg.Logger
	log.Info("polling Cuirass master evaluations",
		"server", p.client.BaseURL(),
		"limit", p.cfg.Limit,
		"systems", p.cfg.Systems,
	)

	masterEvals, err := p.client.LatestEvaluations(ctx, "master", p.cfg.Limit)
	if err != nil {
		log.Error("failed to fetch master evaluations", "err", err)
		return err
	}

	records := make([]store.EvaluationRecord, 0, len(masterEvals))
	for _, ev := range masterEvals {
		records = append(records, store.EvaluationRecord{
			ID:        ev.ID,
			Commit:    ev.PrimaryCommit(),
			Timestamp: ev.StartTime(),
		})
	}
	p.store.UpdateEvaluations(records, p.cfg.Limit*2)

	p.mu.Lock()
	p.cycleCounter++
	cycle := p.cycleCounter
	// Prune cycle tracking entries for evaluations that have aged out of the window.
	activeKeys := make(map[store.EvalSystemKey]bool, len(records)*len(p.cfg.Systems))
	for _, ev := range records {
		for _, sys := range p.cfg.Systems {
			activeKeys[store.EvalSystemKey{EvalID: ev.ID, System: sys}] = true
		}
	}
	for k := range p.refreshedCycle {
		if !activeKeys[k] {
			delete(p.refreshedCycle, k)
		}
	}
	p.mu.Unlock()

	// Fetch recent "guix" jobset evaluations to match commits for `guix pull` substitute status.
	guixEvalByCommit := make(map[string]int64)
	if guixEvals, err := p.client.LatestEvaluations(ctx, "guix", p.cfg.Limit*2); err == nil {
		for _, gev := range guixEvals {
			commit := gev.PrimaryCommit()
			if commit != "" && commit != "unknown" {
				// Keep the newest guix evaluation ID per commit.
				if existing, ok := guixEvalByCommit[commit]; !ok || gev.ID > existing {
					guixEvalByCommit[commit] = gev.ID
				}
			}
		}
	} else {
		log.Warn("failed to fetch guix jobset evaluations", "err", err)
	}

	var updatedCount int

	for _, sys := range p.cfg.Systems {
		// Update guix pull status for evaluations in the window that are not yet ready.
		// Pull readiness is independent of master evaluation completeness: a commit whose
		// master package collection is already complete (0 pending) might still have its
		// 'guix' jobset build in progress, or vice-versa.
		var pullStatusChanged bool
		for _, ev := range records {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			pullEvID, hasPullEval := guixEvalByCommit[ev.Commit]
			cachedPull, hasCachedPull := p.store.GetPullStatus(ev.Commit, sys)
			if !hasPullEval && hasCachedPull && cachedPull.EvalID > 0 {
				pullEvID = cachedPull.EvalID
				hasPullEval = true
			}
			if hasPullEval {
				if !hasCachedPull || !cachedPull.Ready || cachedPull.EvalID != pullEvID {
					pullReady := p.checkGuixPullReady(ctx, pullEvID, sys)
					p.store.SetPullStatus(ev.Commit, sys, pullEvID, pullReady)
					if !hasCachedPull || cachedPull.Ready != pullReady || cachedPull.EvalID != pullEvID {
						pullStatusChanged = true
					}
				}
			}
		}
		if pullStatusChanged {
			p.store.PublishSnapshot()
		}

		var uncachedEvals []store.EvaluationRecord
		var incompleteEvals []store.EvaluationRecord

		for _, ev := range records {
			if p.store.IsEvalSystemComplete(ev.ID, sys) {
				continue
			}
			if !p.store.HasEvalSystem(ev.ID, sys) {
				uncachedEvals = append(uncachedEvals, ev)
			} else {
				incompleteEvals = append(incompleteEvals, ev)
			}
		}

		// Select which incomplete evaluations to refresh this cycle.
		// By default (MaxActiveRefresh <= 0), all incomplete evaluations in the window are refreshed.
		// If MaxActiveRefresh > 0 and there are more incomplete evaluations than MaxActiveRefresh,
		// sort by:
		//  1. Least-recently refreshed cycle (round-robin fairness so older evaluations never starve).
		//  2. Fewest pending jobs (evaluations closest to completion finish first).
		//  3. Newest evaluation ID.
		var toRefreshIncomplete []store.EvaluationRecord
		if p.cfg.MaxActiveRefresh <= 0 || len(incompleteEvals) <= p.cfg.MaxActiveRefresh {
			toRefreshIncomplete = incompleteEvals
		} else {
			snap := p.store.Snapshot()
			p.mu.RLock()
			sortedIncomplete := append([]store.EvaluationRecord(nil), incompleteEvals...)
			sort.Slice(sortedIncomplete, func(i, j int) bool {
				keyI := store.EvalSystemKey{EvalID: sortedIncomplete[i].ID, System: sys}
				keyJ := store.EvalSystemKey{EvalID: sortedIncomplete[j].ID, System: sys}
				cycleI := p.refreshedCycle[keyI]
				cycleJ := p.refreshedCycle[keyJ]
				if cycleI != cycleJ {
					return cycleI < cycleJ // least recently refreshed first
				}
				var pendingI, pendingJ int
				if snap != nil {
					if dataI, ok := snap.ByEvalSystem[keyI]; ok {
						pendingI = dataI.PendingJobs
					}
					if dataJ, ok := snap.ByEvalSystem[keyJ]; ok {
						pendingJ = dataJ.PendingJobs
					}
				}
				if pendingI != pendingJ {
					return pendingI < pendingJ // fewest pending jobs first
				}
				return sortedIncomplete[i].ID > sortedIncomplete[j].ID
			})
			p.mu.RUnlock()
			toRefreshIncomplete = sortedIncomplete[:p.cfg.MaxActiveRefresh]
		}

		evalsToFetch := append(uncachedEvals, toRefreshIncomplete...)

		for _, ev := range evalsToFetch {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			jobs, err := p.client.EvaluationJobs(ctx, ev.ID, sys)
			if err != nil {
				log.Warn("failed to fetch evaluation jobs",
					"eval_id", ev.ID,
					"system", sys,
					"err", err,
				)
				continue
			}

			p.mu.Lock()
			p.refreshedCycle[store.EvalSystemKey{EvalID: ev.ID, System: sys}] = cycle
			p.mu.Unlock()

			p.store.UpsertSystemEval(ev.ID, ev.Commit, sys, jobs)
			updatedCount++

			// Publish incremental in-memory snapshot so HTTP clients can query immediately
			// without serializing the entire database to disk on every evaluation.
			p.store.PublishSnapshot()
		}
	}

	if err := p.store.Save(); err != nil {
		log.Warn("failed to save store state", "err", err)
	}

	log.Info("completed Cuirass poll cycle",
		"evaluations", len(records),
		"updated_eval_systems", updatedCount,
	)
	return nil
}

// checkGuixPullReady returns true if the "guix" jobset evaluation for a commit
// has at least one job for system and all jobs for system succeeded.
func (p *Poller) checkGuixPullReady(ctx context.Context, pullEvalID int64, system string) bool {
	jobs, err := p.client.EvaluationJobs(ctx, pullEvalID, system)
	if err != nil || len(jobs) == 0 {
		return false
	}
	for _, j := range jobs {
		if !j.Status.IsSucceeded() {
			return false
		}
	}
	return true
}

// ResolveMissingVersions lazily fetches /build/<id> for any multi-job packages in the requested
// spec list that do not yet have cached version strings.
//
// Why lazy resolution is needed:
// Cuirass's /api/jobs endpoint returns job names like "gcc-toolchain.x86_64-linux" without
// version numbers. When a package has multiple versions in the same evaluation (e.g. GCC 13
// and GCC 14), the version can only be determined by inspecting the store derivation path
// returned by /build/<id>.
//
// Optimization:
//   - Single-job packages never need version lookups.
//   - Multi-job packages where the user did not request a specific "@version" AND all
//     versions succeeded also skip /build/<id> lookups, because the latest version is
//     guaranteed to have succeeded too.
//   - Resolved build ID -> nixname mappings are permanently cached in the Store across restarts.
func (p *Poller) ResolveMissingVersions(ctx context.Context, system string, specs []string) {
	if len(specs) == 0 {
		return
	}
	snap := p.store.Snapshot()
	if snap == nil {
		return
	}

	// Collect unique build IDs across candidate evaluations that need resolution.
	buildBaseMap := make(map[int64]string)
	for _, ev := range snap.Evaluations {
		data, ok := snap.ByEvalSystem[store.EvalSystemKey{EvalID: ev.ID, System: system}]
		if !ok || data.JobsByBase == nil {
			continue
		}
		for _, spec := range specs {
			basePkg, reqVer := cuirass.SpecToNameAndVer(spec)
			jobs := data.JobsByBase[basePkg]
			if len(jobs) <= 1 {
				continue
			}
			allSucceeded := true
			for _, j := range jobs {
				if !j.Status.IsSucceeded() {
					allSucceeded = false
					break
				}
			}
			if reqVer == "" && allSucceeded {
				continue
			}

			for _, j := range jobs {
				if j.BuildID <= 0 {
					continue
				}
				if _, ok := p.store.GetBuildNixName(j.BuildID); !ok {
					buildBaseMap[j.BuildID] = basePkg
				}
			}
		}
	}

	if len(buildBaseMap) == 0 {
		return
	}

	type buildResult struct {
		id  int64
		nix string
	}
	resCh := make(chan buildResult, len(buildBaseMap))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup

	for buildID := range buildBaseMap {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			b, err := p.client.Build(ctx, id)
			if err != nil {
				return
			}
			nix := b.ResolvedNixName()
			if nix != "" {
				resCh <- buildResult{id: id, nix: nix}
			}
		}(buildID)
	}

	wg.Wait()
	close(resCh)

	newNixes := make(map[int64]string)
	for res := range resCh {
		newNixes[res.id] = res.nix
	}

	if len(newNixes) > 0 {
		p.store.SetBuildNixNames(newNixes)
		_ = p.store.Save()
	}
}
