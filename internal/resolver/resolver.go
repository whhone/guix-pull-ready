// Package resolver implements the commit resolution engine over an in-memory store Snapshot.
//
// It supports two resolution modes:
//   - Whole-Evaluation Mode: scans recent master evaluations from newest to oldest and selects
//     the newest evaluation whose pending build count is at most MaxPending (default 0).
//   - Package Mode: filters out packages not tracked by upstream CI (such as custom or local
//     channel packages), and selects the newest evaluation where every tracked package in the
//     query has a ready substitute.
package resolver

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"guix-evaluated/internal/cuirass"
	"guix-evaluated/internal/store"
)

var (
	ErrNoEvaluations     = errors.New("no evaluations available in store")
	ErrNoReadyEvaluation = errors.New("no matching ready evaluation found within limit")
	ErrNoTrackedPackages = errors.New("none of the specified packages are tracked on CI")
)

// Mode indicates whether a query was evaluated in whole-evaluation or package mode.
type Mode string

const (
	ModeWholeEvaluation Mode = "whole-evaluation"
	ModePackage         Mode = "package"
)

// Query defines parameters for finding a ready Guix commit.
type Query struct {
	System      string
	Limit       int
	MaxPending  int
	AllowFailed bool
	RequirePull *bool // nil defaults to true
	Packages    []string
}

// Bool returns a pointer to the given boolean, useful for setting Query.RequirePull.
func Bool(b bool) *bool {
	return &b
}

// Result contains the resolved Guix commit and evaluation metadata.
type Result struct {
	Commit            string    `json:"commit"`
	EvaluationID      int64     `json:"evaluation_id"`
	EvaluationDate    time.Time `json:"evaluation_date"`
	EvaluationsBehind int       `json:"evaluations_behind"`
	System            string    `json:"system"`
	Mode              Mode      `json:"mode"`
	PendingCount      int       `json:"pending_count"`
	FailedCount       int       `json:"failed_count"`
	SucceededCount    int       `json:"succeeded_count"`
	TotalCount        int       `json:"total_count"`
	PullReady         bool      `json:"pull_ready"`
	PullEvaluationID  int64     `json:"pull_evaluation_id,omitempty"`
	TrackedPackages   int       `json:"tracked_packages,omitempty"`
	UntrackedPackages []string  `json:"untracked_packages"`
	ServerURL         string    `json:"server_url"`
	LastSyncedAt      time.Time `json:"last_synced_at"`
}

// Resolve executes the query against an immutable store Snapshot.
func Resolve(snap *store.Snapshot, q Query) (*Result, error) {
	if snap == nil || len(snap.Evaluations) == 0 {
		return nil, ErrNoEvaluations
	}
	if q.System == "" {
		q.System = "x86_64-linux"
	}
	if q.Limit <= 0 {
		q.Limit = 40
	}
	if q.MaxPending < 0 {
		q.MaxPending = 0
	}

	specs := normalizeSpecs(q.Packages)
	evals := snap.Evaluations
	if len(evals) > q.Limit {
		evals = evals[:q.Limit]
	}

	if len(specs) > 0 {
		return resolvePackageMode(snap, evals, q, specs)
	}
	return resolveWholeEvalMode(snap, evals, q)
}

func resolveWholeEvalMode(snap *store.Snapshot, evals []store.EvaluationRecord, q Query) (*Result, error) {
	requirePull := true
	if q.RequirePull != nil {
		requirePull = *q.RequirePull
	}
	for idx, ev := range evals {
		data, ok := snap.ByEvalSystem[store.EvalSystemKey{EvalID: ev.ID, System: q.System}]
		if !ok || data.TotalJobs == 0 {
			continue
		}
		if requirePull && !data.PullReady {
			continue
		}
		if data.PendingJobs <= q.MaxPending {
			return &Result{
				Commit:            ev.Commit,
				EvaluationID:      ev.ID,
				EvaluationDate:    ev.Timestamp,
				EvaluationsBehind: idx,
				System:            q.System,
				Mode:              ModeWholeEvaluation,
				PendingCount:      data.PendingJobs,
				FailedCount:       data.FailedJobs,
				SucceededCount:    data.SucceededJobs,
				TotalCount:        data.TotalJobs,
				PullReady:         data.PullReady,
				PullEvaluationID:  data.PullEvalID,
				UntrackedPackages: []string{},
				ServerURL:         snap.ServerURL,
				LastSyncedAt:      snap.LastSyncedAt,
			}, nil
		}
	}
	return nil, ErrNoReadyEvaluation
}

type specStatus int

const (
	specStatusSucceeded specStatus = iota
	specStatusPending
	specStatusFailed
	specStatusMissing
)

// resolvePackageMode partitions the requested package specs into tracked (present in at least
// one cached CI evaluation for the target system) and untracked (custom/local packages not built
// by upstream Cuirass). Untracked specs are reported in Result.UntrackedPackages and skipped so
// users piping `guix package --list-installed` are not blocked by third-party channel packages.
func resolvePackageMode(snap *store.Snapshot, evals []store.EvaluationRecord, q Query, specs []string) (*Result, error) {
	trackedMap := snap.TrackedBasePackages[q.System]
	var trackedSpecs []string
	var untrackedSpecs []string

	for _, s := range specs {
		name, _ := cuirass.SpecToNameAndVer(s)
		if trackedMap != nil && trackedMap[name] {
			trackedSpecs = append(trackedSpecs, s)
		} else {
			untrackedSpecs = append(untrackedSpecs, s)
		}
	}

	if len(trackedSpecs) == 0 {
		return nil, ErrNoTrackedPackages
	}
	if untrackedSpecs == nil {
		untrackedSpecs = []string{}
	}

	requirePull := true
	if q.RequirePull != nil {
		requirePull = *q.RequirePull
	}

	for idx, ev := range evals {
		data, ok := snap.ByEvalSystem[store.EvalSystemKey{EvalID: ev.ID, System: q.System}]
		if !ok || data.TotalJobs == 0 {
			continue
		}
		if requirePull && !data.PullReady {
			continue
		}

		var pendingSpecs, failedSpecs, succeededSpecs int
		allReady := true

		for _, spec := range trackedSpecs {
			st := evaluateSpecStatus(spec, q.System, data, q.AllowFailed)
			switch st {
			case specStatusSucceeded:
				succeededSpecs++
			case specStatusPending:
				pendingSpecs++
				allReady = false
			case specStatusFailed:
				failedSpecs++
				allReady = false
			case specStatusMissing:
				allReady = false
			}
		}

		if allReady {
			return &Result{
				Commit:            ev.Commit,
				EvaluationID:      ev.ID,
				EvaluationDate:    ev.Timestamp,
				EvaluationsBehind: idx,
				System:            q.System,
				Mode:              ModePackage,
				PendingCount:      pendingSpecs,
				FailedCount:       failedSpecs,
				SucceededCount:    succeededSpecs,
				TotalCount:        len(trackedSpecs),
				PullReady:         data.PullReady,
				PullEvaluationID:  data.PullEvalID,
				TrackedPackages:   len(trackedSpecs),
				UntrackedPackages: untrackedSpecs,
				ServerURL:         snap.ServerURL,
				LastSyncedAt:      snap.LastSyncedAt,
			}, nil
		}
	}
	return nil, ErrNoReadyEvaluation
}

// evaluateSpecStatus checks a single package spec against an evaluation's job index.
// It checks the O(1) SingleJobs map first (covering ~99% of packages), falling back
// to ResolveJobsForSpec on JobsByBase for multi-version packages.
func evaluateSpecStatus(spec string, targetSystem string, data *store.SystemEvalData, allowFailed bool) specStatus {
	name, _ := cuirass.SpecToNameAndVer(spec)
	if name == "" || data == nil {
		return specStatusMissing
	}
	if data.SingleJobs != nil {
		if rawStatus, ok := data.SingleJobs[name]; ok {
			st := cuirass.JobStatus(rawStatus)
			if st.IsPending() {
				return specStatusPending
			}
			if st.IsFailed() {
				if allowFailed {
					return specStatusSucceeded
				}
				return specStatusFailed
			}
			return specStatusSucceeded
		}
	}
	matched := ResolveJobsForSpec(spec, targetSystem, data.JobsByBase)
	return classifySpecJobs(matched, allowFailed)
}

func classifySpecJobs(matched []cuirass.Job, allowFailed bool) specStatus {
	if len(matched) == 0 {
		return specStatusMissing
	}
	var hasPending, hasFailed bool
	for _, j := range matched {
		if j.Status.IsPending() {
			hasPending = true
		} else if j.Status.IsFailed() {
			hasFailed = true
		}
	}
	if hasPending {
		return specStatusPending
	}
	if hasFailed {
		if allowFailed {
			return specStatusSucceeded
		}
		return specStatusFailed
	}
	return specStatusSucceeded
}

// ResolveJobsForSpec filters and resolves the jobs matching a package spec (e.g. "gcc-toolchain@14").
// Matches resolve-jobs-for-spec in guix-sync-commit.scm:
//  1. Matches both "<name>.<system>" and "base.<name>.<system>" job names.
//  2. If no "@version" prefix is requested and all matching jobs succeeded, returns all of them immediately.
//  3. If only a single job matched, returns it.
//  4. If multiple jobs matched and "@version" was specified, returns jobs whose Version starts with reqVer
//     (or all matched jobs if versions have not yet been resolved).
//  5. If multiple jobs matched and no "@version" was specified, selects the single job with the highest
//     version according to cuirass.VersionGreater (matching Guix's default behaviour of picking the newest version).
func ResolveJobsForSpec(spec string, targetSystem string, jobsByBase map[string][]cuirass.Job) []cuirass.Job {
	name, reqVer := cuirass.SpecToNameAndVer(spec)
	if name == "" || jobsByBase == nil {
		return nil
	}
	candidates := jobsByBase[name]
	if len(candidates) == 0 {
		return nil
	}

	targetSuffix := "." + targetSystem
	exactName := name + targetSuffix
	baseName := "base." + name + targetSuffix

	matched := make([]cuirass.Job, 0, len(candidates))
	for _, j := range candidates {
		if j.Name == exactName || j.Name == baseName {
			matched = append(matched, j)
		}
	}
	if len(matched) == 0 {
		return nil
	}

	// Fast path: if all matching jobs succeeded and no version filter is requested, it is ready.
	if reqVer == "" {
		allSucceeded := true
		for _, j := range matched {
			if !j.Status.IsSucceeded() {
				allSucceeded = false
				break
			}
		}
		if allSucceeded {
			return matched
		}
	}

	// Only a single job exists for this package name.
	if len(matched) == 1 {
		return matched
	}

	// Multiple jobs exist (e.g. multiple versions of gcc-toolchain or linux-libre):
	// resolve versions using j.Version.
	if reqVer != "" {
		var filtered []cuirass.Job
		hasUnknown := false
		for _, j := range matched {
			if j.Version == "" {
				hasUnknown = true
			} else if strings.HasPrefix(j.Version, reqVer) {
				filtered = append(filtered, j)
			}
		}
		if len(filtered) > 0 {
			return filtered
		}
		if hasUnknown {
			return matched
		}
		return nil
	}

	// Pick the job with the latest version according to VersionGreater.
	sorted := make([]cuirass.Job, len(matched))
	copy(sorted, matched)
	sort.SliceStable(sorted, func(i, j int) bool {
		return cuirass.VersionGreater(sorted[i].Version, sorted[j].Version)
	})
	return sorted[:1]
}

// FormatChannelsSCM renders a ready-to-use channels.scm snippet with evaluation metadata comments.
func FormatChannelsSCM(r *Result) string {
	var b strings.Builder
	dateStr := "unknown"
	if !r.EvaluationDate.IsZero() {
		dateStr = r.EvaluationDate.UTC().Format("2006-01-02 15:04 UTC")
	}
	lagStr := "latest evaluation"
	if r.EvaluationsBehind == 1 {
		lagStr = "1 evaluation behind master"
	} else if r.EvaluationsBehind > 1 {
		lagStr = fmt.Sprintf("%d evaluations behind master", r.EvaluationsBehind)
	}
	pullStatus := "Building or pending in 'guix' jobset; expected local compile of Guix modules"
	if r.PullReady {
		pullStatus = "Ready (prebuilt binary available for Guix itself)"
	}

	fmt.Fprintf(&b, ";; Generated by guix-pull-ready service\n")
	fmt.Fprintf(&b, ";; Newest ready commit: %s\n", r.Commit)
	fmt.Fprintf(&b, ";; System:              %s\n", r.System)
	fmt.Fprintf(&b, ";; Date:                %s\n", dateStr)
	fmt.Fprintf(&b, ";; Evaluation:          %d (spec: master, %s)\n", r.EvaluationID, lagStr)
	if r.PullEvaluationID > 0 {
		fmt.Fprintf(&b, ";; Guix Jobset Eval:    %d (jobset: guix)\n", r.PullEvaluationID)
	}
	if len(r.UntrackedPackages) > 0 {
		fmt.Fprintf(&b, ";; Skipped local:       %s\n", strings.Join(r.UntrackedPackages, " "))
	}
	fmt.Fprintf(&b, ";; guix pull status:    %s\n", pullStatus)
	fmt.Fprintf(&b, "(list (channel\n")
	fmt.Fprintf(&b, "       (inherit %%default-guix-channel)\n")
	fmt.Fprintf(&b, "       (url \"https://codeberg.org/guix/guix.git\")\n")
	fmt.Fprintf(&b, "       (commit \"%s\")\n", r.Commit)
	fmt.Fprintf(&b, "       (introduction\n")
	fmt.Fprintf(&b, "        (make-channel-introduction\n")
	fmt.Fprintf(&b, "         \"9edb3f66fd807b096b48283debdcddccfea34bad\"\n")
	fmt.Fprintf(&b, "         (openpgp-fingerprint\n")
	fmt.Fprintf(&b, "          \"BBB0 2DDF 2CEA F6A8 0D1D  E643 A2A0 6DF2 A33A 54FA\")))))\n")
	return b.String()
}

// normalizeSpecs splits comma- or whitespace-separated package spec strings,
// ignores blank lines and '#' or ';' comment lines, and deduplicates preserving order.
func normalizeSpecs(raw []string) []string {
	seen := make(map[string]bool, len(raw))
	var out []string
	for _, item := range raw {
		for _, tok := range strings.FieldsFunc(item, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
		}) {
			tok = strings.TrimSpace(tok)
			if tok == "" || strings.HasPrefix(tok, "#") || strings.HasPrefix(tok, ";") {
				continue
			}
			if !seen[tok] {
				seen[tok] = true
				out = append(out, tok)
			}
		}
	}
	return out
}
