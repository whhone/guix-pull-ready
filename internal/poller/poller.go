// Package poller synchronizes evaluation and job states from an upstream
// Cuirass server in the background and lazily resolves build version strings
// for multi-version packages.
package poller

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"guix-evaluated/internal/cuirass"
	"guix-evaluated/internal/store"
)

// ChannelConfig configures an additional channel Cuirass jobset (such as nonguix)
// to poll alongside the primary Guix instance.
type ChannelConfig struct {
	Name   string          // Channel name (e.g. "nonguix")
	Spec   string          // Cuirass specification/jobset name (e.g. "nonguix")
	Client *cuirass.Client // Cuirass API client for this channel's build farm
}

// Config holds configuration for the background Cuirass poller.
type Config struct {
	Systems             []string
	Limit               int
	PollInterval        time.Duration
	MaxActiveRefresh    int
	MaxReconcilePending int
	AdditionalChannels  []ChannelConfig
	Logger              *slog.Logger
}

// Poller periodically synchronizes evaluations and job statuses from Cuirass into the local Store.
type Poller struct {
	client *cuirass.Client
	store  *store.Store
	cfg    Config

	mu                    sync.RWMutex
	lastPollErr           error
	lastPollEnd           time.Time
	syncing               bool
	refreshedCycle        map[store.EvalSystemKey]uint64
	channelRefreshedCycle map[store.ChannelEvalSystemKey]uint64
	reconciledBuilds      map[store.ChannelBuildKey]cuirass.JobStatus
	cycleCounter          uint64
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
	if st != nil {
		for _, ch := range cfg.AdditionalChannels {
			if ch.Name != "" && ch.Client != nil {
				st.SetChannelServerURL(ch.Name, ch.Client.BaseURL())
			}
		}
	}
	return &Poller{
		client:                client,
		store:                 st,
		cfg:                   cfg,
		refreshedCycle:        make(map[store.EvalSystemKey]uint64),
		channelRefreshedCycle: make(map[store.ChannelEvalSystemKey]uint64),
		reconciledBuilds:      make(map[store.ChannelBuildKey]cuirass.JobStatus),
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

	p.mu.Lock()
	p.cycleCounter++
	cycle := p.cycleCounter
	p.mu.Unlock()

	var updatedCount int

	// 1. Poll any configured additional channels (such as nonguix) first so we know
	// which base Guix commits they reference in their checkouts.
	for _, ch := range p.cfg.AdditionalChannels {
		if ch.Name == "" || ch.Client == nil {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		nUpdated := p.syncAdditionalChannel(ctx, ch, cycle)
		updatedCount += nUpdated
	}

	referencedGuixCommits := make(map[string]bool)
	if snap := p.store.Snapshot(); snap != nil {
		for _, chEvals := range snap.ChannelEvaluations {
			for _, ev := range chEvals {
				if c := ev.Checkouts["guix"]; c != "" {
					referencedGuixCommits[c] = true
				}
			}
		}
	}

	masterFetchLimit := p.cfg.Limit
	if len(referencedGuixCommits) > 0 && masterFetchLimit < 100 {
		masterFetchLimit = 100
	}

	log.Info("polling Cuirass master evaluations",
		"server", p.client.BaseURL(),
		"limit", p.cfg.Limit,
		"systems", p.cfg.Systems,
	)

	masterEvals, err := p.client.LatestEvaluations(ctx, "master", masterFetchLimit)
	if err != nil {
		log.Error("failed to fetch master evaluations", "err", err)
		return err
	}

	records := make([]store.EvaluationRecord, 0, p.cfg.Limit+len(referencedGuixCommits))
	for idx, ev := range masterEvals {
		commit := ev.PrimaryCommit()
		if idx < p.cfg.Limit || referencedGuixCommits[commit] {
			records = append(records, store.EvaluationRecord{
				ID:        ev.ID,
				Channel:   "guix",
				Commit:    commit,
				Checkouts: ev.CheckoutsMap(),
				Timestamp: ev.StartTime(),
			})
		}
	}
	p.store.UpdateEvaluations(records, p.cfg.Limit*2)
	for i := range records {
		commit := records[i].Commit
		if commit == "" || commit == "unknown" {
			continue
		}
		if ct, ok := p.store.GetCommitTime(commit); ok {
			records[i].Timestamp = ct
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if ct, err := p.client.CommitTimestamp(ctx, commit); err == nil && !ct.IsZero() {
			p.store.SetCommitTime(commit, ct)
			records[i].Timestamp = ct
		}
	}
	p.store.PublishSnapshot()
	if snap := p.store.Snapshot(); snap != nil && len(snap.Evaluations) > len(records) {
		records = snap.Evaluations
	}

	p.mu.Lock()
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
	guixPullLimit := p.cfg.Limit * 2
	if masterFetchLimit*2 > guixPullLimit {
		guixPullLimit = masterFetchLimit * 2
	}
	guixEvalByCommit := make(map[string]int64)
	if guixEvals, err := p.client.LatestEvaluations(ctx, "guix", guixPullLimit); err == nil {
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

			p.reconcilePendingJobs(ctx, p.client, "guix", sys, jobs)

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

// syncAdditionalChannel polls evaluations and job statuses for an additional channel
// (such as nonguix). Errors are logged as warnings so primary Guix polling is unaffected.
func (p *Poller) syncAdditionalChannel(ctx context.Context, ch ChannelConfig, cycle uint64) int {
	log := p.cfg.Logger
	spec := ch.Spec
	if spec == "" {
		spec = ch.Name
	}

	log.Info("polling additional channel evaluations",
		"channel", ch.Name,
		"server", ch.Client.BaseURL(),
		"spec", spec,
		"limit", p.cfg.Limit,
	)

	evals, err := ch.Client.LatestEvaluations(ctx, spec, p.cfg.Limit)
	if err != nil {
		log.Warn("failed to fetch additional channel evaluations",
			"channel", ch.Name,
			"server", ch.Client.BaseURL(),
			"err", err,
		)
		return 0
	}

	records := make([]store.EvaluationRecord, 0, len(evals))
	for _, ev := range evals {
		commit := ev.CheckoutCommit(ch.Name)
		if commit == "" {
			commit = ev.PrimaryCommit()
		}
		records = append(records, store.EvaluationRecord{
			ID:        ev.ID,
			Channel:   ch.Name,
			Commit:    commit,
			Checkouts: ev.CheckoutsMap(),
			Timestamp: ev.StartTime(),
		})
	}
	p.store.UpdateChannelEvaluations(ch.Name, records, p.cfg.Limit*2)
	p.store.PublishSnapshot()

	p.mu.Lock()
	activeKeys := make(map[store.ChannelEvalSystemKey]bool, len(records)*len(p.cfg.Systems))
	for _, ev := range records {
		for _, sys := range p.cfg.Systems {
			activeKeys[store.ChannelEvalSystemKey{Channel: ch.Name, EvalID: ev.ID, System: sys}] = true
		}
	}
	for k := range p.channelRefreshedCycle {
		if k.Channel == ch.Name && !activeKeys[k] {
			delete(p.channelRefreshedCycle, k)
		}
	}
	p.mu.Unlock()

	var updatedCount int
	for _, sys := range p.cfg.Systems {
		var uncachedEvals []store.EvaluationRecord
		var incompleteEvals []store.EvaluationRecord

		for _, ev := range records {
			if p.store.IsChannelEvalSystemComplete(ch.Name, ev.ID, sys) {
				continue
			}
			if !p.store.HasChannelEvalSystem(ch.Name, ev.ID, sys) {
				uncachedEvals = append(uncachedEvals, ev)
			} else {
				incompleteEvals = append(incompleteEvals, ev)
			}
		}

		var toRefreshIncomplete []store.EvaluationRecord
		if p.cfg.MaxActiveRefresh <= 0 || len(incompleteEvals) <= p.cfg.MaxActiveRefresh {
			toRefreshIncomplete = incompleteEvals
		} else {
			snap := p.store.Snapshot()
			p.mu.RLock()
			sortedIncomplete := append([]store.EvaluationRecord(nil), incompleteEvals...)
			sort.Slice(sortedIncomplete, func(i, j int) bool {
				keyI := store.ChannelEvalSystemKey{Channel: ch.Name, EvalID: sortedIncomplete[i].ID, System: sys}
				keyJ := store.ChannelEvalSystemKey{Channel: ch.Name, EvalID: sortedIncomplete[j].ID, System: sys}
				cycleI := p.channelRefreshedCycle[keyI]
				cycleJ := p.channelRefreshedCycle[keyJ]
				if cycleI != cycleJ {
					return cycleI < cycleJ
				}
				var pendingI, pendingJ int
				if snap != nil {
					if dataI, ok := snap.ByChannelEvalSystem[keyI]; ok {
						pendingI = dataI.PendingJobs
					}
					if dataJ, ok := snap.ByChannelEvalSystem[keyJ]; ok {
						pendingJ = dataJ.PendingJobs
					}
				}
				if pendingI != pendingJ {
					return pendingI < pendingJ
				}
				return sortedIncomplete[i].ID > sortedIncomplete[j].ID
			})
			p.mu.RUnlock()
			toRefreshIncomplete = sortedIncomplete[:p.cfg.MaxActiveRefresh]
		}

		evalsToFetch := append(uncachedEvals, toRefreshIncomplete...)
		for _, ev := range evalsToFetch {
			if ctx.Err() != nil {
				return updatedCount
			}
			jobs, err := ch.Client.EvaluationJobs(ctx, ev.ID, sys)
			if err != nil {
				log.Warn("failed to fetch channel evaluation jobs",
					"channel", ch.Name,
					"eval_id", ev.ID,
					"system", sys,
					"err", err,
				)
				continue
			}

			p.reconcilePendingJobs(ctx, ch.Client, ch.Name, sys, jobs)

			p.mu.Lock()
			p.channelRefreshedCycle[store.ChannelEvalSystemKey{Channel: ch.Name, EvalID: ev.ID, System: sys}] = cycle
			p.mu.Unlock()

			p.store.UpsertChannelSystemEval(ch.Name, ev.ID, ev.Commit, sys, jobs)
			updatedCount++
			p.store.PublishSnapshot()
		}
	}

	return updatedCount
}

// reconcilePendingJobs cross-checks pending jobs against Cuirass /build/<id> endpoints
// to reconcile upstream database desynchronization.
//
// The Problem:
// In upstream Cuirass (ci.guix.gnu.org), evaluations register jobs into a denormalized
// `Jobs` table, which is kept in sync with the canonical `Builds` table via PostgreSQL
// triggers. When a build completes under another jobset (such as "science-team") or concurrently
// during evaluation registration, the completion trigger updates existing evaluation rows but can
// miss the newly inserted job row. Because Cuirass never issues another UPDATE on `Builds` for
// that derivation once completed, Cuirass's /api/jobs endpoint permanently reports the job as
// scheduled or pending (e.g. status -3 or -1), even though the underlying derivation was already
// built successfully.
//
// How Reconciliation Works:
//  1. When an evaluation for a system is near completion (has 1 to MaxReconcilePending pending jobs),
//     we collect all jobs with IsPending() == true and a valid BuildID > 0.
//  2. For each pending job, we query Cuirass's canonical /build/<id> endpoint (or check our
//     in-memory reconciledBuilds cache).
//  3. If /build/<id> indicates the build is finished (finished == 1):
//     - We override job.Status in-place with JobStatus(build.BuildStatus) (e.g. 0 for succeeded).
//     - We cache the resolved status in reconciledBuilds so future passes avoid re-querying.
//     - We cache any resolved nixname derivation paths for package version resolution.
//  4. If all pending jobs reconcile to succeeded (or failed), the evaluation naturally reaches
//     PendingJobs == 0 and is marked Complete in the Store, permanently unblocking it from
//     blocking whole-evaluation mode.
func (p *Poller) reconcilePendingJobs(ctx context.Context, client *cuirass.Client, channel string, system string, jobs []cuirass.Job) {
	if p.cfg.MaxReconcilePending <= 0 || client == nil || len(jobs) == 0 {
		return
	}
	if channel == "" {
		channel = "guix"
	}

	suffix := "." + system
	type pendingItem struct {
		index   int
		buildID int64
	}

	// 1. Fast path: apply previously reconciled build statuses from cache.
	p.mu.RLock()
	for i := range jobs {
		if !strings.HasSuffix(jobs[i].Name, suffix) {
			continue
		}
		if jobs[i].Status.IsPending() && jobs[i].BuildID > 0 {
			key := store.ChannelBuildKey{Channel: channel, BuildID: jobs[i].BuildID}
			if cachedStatus, ok := p.reconciledBuilds[key]; ok {
				jobs[i].Status = cachedStatus
			}
		}
	}
	p.mu.RUnlock()

	// 2. Collect jobs that remain pending and need Cuirass /build/<id> verification.
	var stillPending []pendingItem
	for i := range jobs {
		if !strings.HasSuffix(jobs[i].Name, suffix) {
			continue
		}
		if jobs[i].Status.IsPending() && jobs[i].BuildID > 0 {
			stillPending = append(stillPending, pendingItem{index: i, buildID: jobs[i].BuildID})
		}
	}

	if len(stillPending) == 0 {
		return
	}

	toCheck := stillPending
	if len(toCheck) > p.cfg.MaxReconcilePending {
		// Only attempt batch reconciliation when the evaluation is near completion
		// (within 2x limit) to avoid probing actively compiling evaluations.
		if len(stillPending) > p.cfg.MaxReconcilePending*2 {
			return
		}
		toCheck = toCheck[:p.cfg.MaxReconcilePending]
	}

	nixMap := make(map[int64]string)
	for _, item := range toCheck {
		if ctx.Err() != nil {
			return
		}

		key := store.ChannelBuildKey{Channel: channel, BuildID: item.buildID}
		b, err := client.Build(ctx, item.buildID)
		if err != nil {
			p.cfg.Logger.Debug("failed to query build for pending job reconciliation",
				"channel", channel,
				"build_id", item.buildID,
				"job", jobs[item.index].Name,
				"err", err,
			)
			continue
		}

		if nix := b.ResolvedNixName(); nix != "" && b.ID > 0 {
			nixMap[b.ID] = nix
		}

		if b.IsFinished() {
			status := b.Status()
			jobs[item.index].Status = status

			p.mu.Lock()
			p.reconciledBuilds[key] = status
			p.mu.Unlock()

			p.cfg.Logger.Info("reconciled stuck pending job via /build/<id>",
				"channel", channel,
				"build_id", item.buildID,
				"job", jobs[item.index].Name,
				"original_status", "pending",
				"reconciled_status", status,
			)
		}
	}

	if len(nixMap) > 0 && p.store != nil {
		p.store.SetChannelBuildNixNames(channel, nixMap)
	}
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
func (p *Poller) ResolveMissingVersions(ctx context.Context, system string, specs []string, additionalChannels ...string) {
	if len(specs) == 0 {
		return
	}
	snap := p.store.Snapshot()
	if snap == nil {
		return
	}

	var anyUpdated bool
	if p.resolveChannelMissingVersions(ctx, "guix", p.client, snap.Evaluations, func(evalID int64) *store.SystemEvalData {
		return snap.ByEvalSystem[store.EvalSystemKey{EvalID: evalID, System: system}]
	}, specs) {
		anyUpdated = true
	}

	if len(additionalChannels) > 0 {
		chClients := make(map[string]*cuirass.Client, len(p.cfg.AdditionalChannels))
		for _, ch := range p.cfg.AdditionalChannels {
			if ch.Name != "" && ch.Client != nil {
				chClients[ch.Name] = ch.Client
			}
		}
		for _, chName := range additionalChannels {
			client := chClients[chName]
			if client == nil {
				continue
			}
			evals := snap.ChannelEvaluations[chName]
			if len(evals) == 0 {
				continue
			}
			if p.resolveChannelMissingVersions(ctx, chName, client, evals, func(evalID int64) *store.SystemEvalData {
				return snap.ByChannelEvalSystem[store.ChannelEvalSystemKey{Channel: chName, EvalID: evalID, System: system}]
			}, specs) {
				anyUpdated = true
			}
		}
	}

	if anyUpdated {
		_ = p.store.Save()
	}
}

func (p *Poller) resolveChannelMissingVersions(
	ctx context.Context,
	channel string,
	client *cuirass.Client,
	evals []store.EvaluationRecord,
	getData func(evalID int64) *store.SystemEvalData,
	specs []string,
) bool {
	if client == nil || len(evals) == 0 {
		return false
	}

	buildBaseMap := make(map[int64]string)
	for _, ev := range evals {
		data := getData(ev.ID)
		if data == nil || data.JobsByBase == nil {
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
				if _, ok := p.store.GetChannelBuildNixName(channel, j.BuildID); !ok {
					buildBaseMap[j.BuildID] = basePkg
				}
			}
		}
	}

	if len(buildBaseMap) == 0 {
		return false
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

			b, err := client.Build(ctx, id)
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
		p.store.SetChannelBuildNixNames(channel, newNixes)
		return true
	}
	return false
}
