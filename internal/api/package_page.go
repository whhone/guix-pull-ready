package api

import (
	"fmt"
	"html"
	"net/url"
	"sort"
	"strings"
	"time"

	"guix-pull-ready/internal/cuirass"
	"guix-pull-ready/internal/resolver"
	"guix-pull-ready/internal/store"
)

type packageJobEntry struct {
	Version string
	Status  cuirass.JobStatus
	BuildID int64
	JobName string
}

type packageEvalRow struct {
	EvaluationID       int64
	Commit             string
	Timestamp          time.Time
	EvalPendingJobs    int
	EvalFailedJobs     int
	PullReady          bool
	PullEvalID         int64
	PullPendingJobs    int
	PullFailedJobs     int
	NonguixEval        channelEvalSummary
	HasNonguixEval     bool
	Channel            string
	Status             string
	Jobs               []packageJobEntry
	PrimaryBuildID     int64
	InspectURL         string
	IsLatestWholeReady bool
	IsLatestPkgReady   bool
	EvaluationsBehind  int
}

type packagePageState struct {
	BaseURL            string
	ServerURL          string
	NonguixServerURL   string
	ActiveSystem       string
	Systems            []string
	RequireNonguix     bool
	LastSyncedAt       time.Time
	RawSpec            string
	BaseName           string
	VersionFilter      string
	TrackedInGuix      bool
	TrackedInNonguix   bool
	PrimaryChannel     string
	LatestWholeReady   *packageEvalRow
	LatestPkgReady     *packageEvalRow
	Evaluations        []packageEvalRow
	SucceededCount     int
	FailedCount        int
	PendingCount       int
	StuckCount         int
	MissingCount       int
}

func selectSystemsAndActive(snap *store.Snapshot, reqSystem string) ([]string, string) {
	defaultSystems := cuirass.DefaultSystems
	var systems []string
	if snap != nil && len(snap.Systems) > 0 {
		seen := make(map[string]bool, len(snap.Systems))
		for _, sys := range snap.Systems {
			if sys != "" && !seen[sys] {
				seen[sys] = true
				systems = append(systems, sys)
			}
		}
	} else {
		systems = append([]string(nil), defaultSystems...)
	}

	active := defaultSystems[0]
	if reqSystem != "" {
		for _, sys := range systems {
			if sys == reqSystem {
				return systems, reqSystem
			}
		}
	}

	hasDefault := false
	for _, sys := range systems {
		if sys == "x86_64-linux" {
			hasDefault = true
			break
		}
	}
	if !hasDefault && len(systems) > 0 {
		active = systems[0]
	}
	return systems, active
}

func selectActiveSystem(snap *store.Snapshot, reqSystem string) string {
	_, active := selectSystemsAndActive(snap, reqSystem)
	return active
}

func buildPackagePageState(baseURL string, snap *store.Snapshot, rawSpec string, reqSystem string, requireNonguix bool) packagePageState {
	systems, activeSys := selectSystemsAndActive(snap, reqSystem)
	baseName, verFilter := cuirass.SpecToNameAndVer(rawSpec)

	st := packagePageState{
		BaseURL:          baseURL,
		ServerURL:        "https://ci.guix.gnu.org",
		NonguixServerURL: cuirass.DefaultChannelServerURL("nonguix"),
		ActiveSystem:     activeSys,
		Systems:          systems,
		RequireNonguix:   requireNonguix,
		RawSpec:          rawSpec,
		BaseName:         baseName,
		VersionFilter:    verFilter,
		PrimaryChannel:   "guix",
	}

	if snap == nil || baseName == "" {
		return st
	}

	st.ServerURL = snap.ServerURL
	if ngURL := snap.ChannelServerURLs["nonguix"]; ngURL != "" {
		st.NonguixServerURL = ngURL
	}
	st.LastSyncedAt = snap.LastSyncedAt

	trackedInGuixMaster := snap.IsPackageTracked("guix", activeSys, baseName)
	trackedInGuixPull := snap.IsPackageTracked(store.GuixJobsetChannel, activeSys, baseName)
	st.TrackedInGuix = trackedInGuixMaster || trackedInGuixPull
	st.TrackedInNonguix = snap.IsPackageTracked("nonguix", activeSys, baseName)

	if !st.TrackedInGuix && st.TrackedInNonguix {
		st.PrimaryChannel = "nonguix"
		st.RequireNonguix = true
	}

	if !st.TrackedInGuix && !st.TrackedInNonguix {
		return st
	}

	limit := 40
	var wholeReadyEvalID int64
	if st.RequireNonguix {
		if res, err := resolver.Resolve(snap, resolver.Query{
			System:             activeSys,
			MaxPending:         0,
			Limit:              limit,
			AdditionalChannels: []string{"nonguix"},
		}); err == nil && res != nil {
			wholeReadyEvalID = res.EvaluationID
		}
	} else {
		if res, err := resolver.Resolve(snap, resolver.Query{
			System:     activeSys,
			MaxPending: 0,
			Limit:      limit,
		}); err == nil && res != nil {
			wholeReadyEvalID = res.EvaluationID
		}
	}

	requirePull := cuirass.SupportsGuixJobset(activeSys)

	for idx, ev := range snap.Evaluations {
		data, ok := snap.ByEvalSystem[store.EvalSystemKey{EvalID: ev.ID, System: activeSys}]
		if !ok || data == nil || data.TotalJobs == 0 {
			continue
		}

		chSum, hasNg := matchChannelEvaluation(snap, "nonguix", ev.Commit, activeSys)
		isWholeReady := wholeReadyEvalID > 0 && ev.ID == wholeReadyEvalID

		if len(st.Evaluations) >= limit && !hasNg && !isWholeReady {
			continue
		}

		var jobs []packageJobEntry
		var statusStr string
		var primaryBuildID int64
		var inspectURL string

		if st.PrimaryChannel == "nonguix" {
			var ngData *store.SystemEvalData
			if hasNg {
				ngData = snap.ByChannelEvalSystem[store.ChannelEvalSystemKey{
					Channel: "nonguix",
					EvalID:  chSum.EvalID,
					System:  activeSys,
				}]
			}
			jobs, statusStr, primaryBuildID = extractPackageJobsForEval(snap, rawSpec, baseName, verFilter, activeSys, ngData)
			if primaryBuildID > 0 {
				inspectURL = fmt.Sprintf("%s/build/%d/details", st.NonguixServerURL, primaryBuildID)
			} else {
				query := fmt.Sprintf("spec:nonguix system:%s %s", activeSys, baseName)
				inspectURL = st.NonguixServerURL + "/search?query=" + url.QueryEscape(query)
			}
		} else {
			evalDataForPkg := data
			searchSpec := "master"
			if !trackedInGuixMaster && trackedInGuixPull {
				searchSpec = "guix"
				evalDataForPkg = nil
				if data.PullEvalID > 0 {
					evalDataForPkg = snap.ByChannelEvalSystem[store.ChannelEvalSystemKey{
						Channel: store.GuixJobsetChannel,
						EvalID:  data.PullEvalID,
						System:  activeSys,
					}]
				}
			}
			jobs, statusStr, primaryBuildID = extractPackageJobsForEval(snap, rawSpec, baseName, verFilter, activeSys, evalDataForPkg)
			if primaryBuildID > 0 {
				inspectURL = fmt.Sprintf("%s/build/%d/details", st.ServerURL, primaryBuildID)
			} else {
				query := fmt.Sprintf("spec:%s system:%s %s", searchSpec, activeSys, baseName)
				inspectURL = st.ServerURL + "/search?query=" + url.QueryEscape(query)
			}
		}

		switch statusStr {
		case "succeeded":
			st.SucceededCount++
		case "scheduled", "submitted", "started":
			st.PendingCount++
		case "stuck":
			st.StuckCount++
		case "not-built":
			st.MissingCount++
		default:
			st.FailedCount++
		}

		pullPending, pullFailed := lookupPullJobCounts(snap, data.PullEvalID, activeSys)
		row := packageEvalRow{
			EvaluationID:       ev.ID,
			Commit:             ev.Commit,
			Timestamp:          ev.Timestamp,
			EvalPendingJobs:    data.PendingJobs,
			EvalFailedJobs:     data.FailedJobs,
			PullReady:          data.PullReady,
			PullEvalID:         data.PullEvalID,
			PullPendingJobs:    pullPending,
			PullFailedJobs:     pullFailed,
			NonguixEval:        chSum,
			HasNonguixEval:     hasNg,
			Channel:            st.PrimaryChannel,
			Status:             statusStr,
			Jobs:               jobs,
			PrimaryBuildID:     primaryBuildID,
			InspectURL:         inspectURL,
			IsLatestWholeReady: isWholeReady,
			EvaluationsBehind:  idx,
		}

		pullOK := !requirePull || data.PullReady
		channelOK := true
		if st.PrimaryChannel == "nonguix" {
			channelOK = hasNg && chSum.EvalID > 0
		} else if st.RequireNonguix {
			channelOK = hasNg && chSum.Ready
		}

		if st.LatestPkgReady == nil && pullOK && channelOK && statusStr == "succeeded" {
			row.IsLatestPkgReady = true
		}

		st.Evaluations = append(st.Evaluations, row)
		rowPtr := &st.Evaluations[len(st.Evaluations)-1]
		if rowPtr.IsLatestWholeReady {
			st.LatestWholeReady = rowPtr
		}
		if rowPtr.IsLatestPkgReady {
			st.LatestPkgReady = rowPtr
		}
	}

	return st
}

func extractPackageJobsForEval(snap *store.Snapshot, rawSpec, baseName, verFilter, sys string, data *store.SystemEvalData) ([]packageJobEntry, string, int64) {
	if data == nil {
		return nil, "not-built", 0
	}

	if verFilter == "" {
		if st, ok := snap.LookupSingleJob(data, baseName); ok {
			buildID := findEvalBuildID(data, baseName, sys)
			entry := packageJobEntry{
				Status:  st,
				BuildID: buildID,
				JobName: baseName + "." + sys,
			}
			return []packageJobEntry{entry}, st.String(), buildID
		}
	}

	if data.JobsByBase != nil {
		candidates := data.JobsByBase[baseName]
		if len(candidates) > 0 {
			targetSuffix := "." + sys
			exactName := baseName + targetSuffix
			prefixedName := "base." + baseName + targetSuffix

			var matched []cuirass.Job
			for _, j := range candidates {
				if j.Name == exactName || j.Name == prefixedName {
					if verFilter != "" && j.Version != "" && !strings.HasPrefix(j.Version, verFilter) {
						continue
					}
					matched = append(matched, j)
				}
			}
			if len(matched) > 0 {
				sort.SliceStable(matched, func(i, j int) bool {
					return cuirass.VersionGreater(matched[i].Version, matched[j].Version)
				})
				entries := make([]packageJobEntry, 0, len(matched))
				for _, j := range matched {
					entries = append(entries, packageJobEntry{
						Version: j.Version,
						Status:  j.Status,
						BuildID: j.BuildID,
						JobName: j.Name,
					})
				}

				resolved := resolver.ResolveJobsForSpec(rawSpec, sys, data.JobsByBase)
				if len(resolved) > 0 {
					// Compute effective status from resolved jobs
					var hasPending, hasStuck, hasFailed bool
					primaryStatus := cuirass.StatusSucceeded
					var primaryBuildID int64
					for _, rj := range resolved {
						if primaryBuildID == 0 && rj.BuildID > 0 {
							primaryBuildID = rj.BuildID
						}
						if rj.Status.IsPending() && !hasPending {
							hasPending = true
							primaryStatus = rj.Status
							if rj.BuildID > 0 {
								primaryBuildID = rj.BuildID
							}
						} else if rj.Status.IsStuck() && !hasPending && !hasStuck {
							hasStuck = true
							primaryStatus = rj.Status
							if rj.BuildID > 0 {
								primaryBuildID = rj.BuildID
							}
						} else if rj.Status.IsFailed() && !hasPending && !hasStuck && !hasFailed {
							hasFailed = true
							primaryStatus = rj.Status
							if rj.BuildID > 0 {
								primaryBuildID = rj.BuildID
							}
						}
					}
					return entries, primaryStatus.String(), primaryBuildID
				}
				return entries, entries[0].Status.String(), entries[0].BuildID
			}
		}
	}

	return nil, "not-built", 0
}

func findEvalBuildID(data *store.SystemEvalData, baseName, sys string) int64 {
	if data == nil {
		return 0
	}
	if id := findPendingBuildID(data.PendingBuilds, baseName, sys); id > 0 {
		return id
	}
	return findPendingBuildID(data.StuckBuilds, baseName, sys)
}

func findPendingBuildID(builds []store.PendingBuild, baseName, sys string) int64 {
	exact := baseName + "." + sys
	prefixed := "base." + exact
	for _, pb := range builds {
		if pb.JobName == exact || pb.JobName == prefixed {
			return pb.BuildID
		}
	}
	return 0
}

func renderHTMLPackagePage(st packagePageState) string {
	var b strings.Builder
	title := fmt.Sprintf("%s - Package Substitute Status", st.RawSpec)
	b.WriteString(renderPageHead(title))
	b.WriteString(renderPackagePageHeader(st))

	b.WriteString(`<main>`)
	querySuffix := html.EscapeString(buildQuerySuffix(st.ActiveSystem, st.RequireNonguix))
	fmt.Fprintf(&b, `<div style="margin-bottom:0.85rem;font-size:0.88rem;display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:0.5rem;">
		<a href="/%s">&larr; Back to Dashboard</a>
		<form onsubmit="return jumpToPackage(this)" style="display:inline-flex;align-items:center;gap:0.4rem;">
			<input type="search" name="pkg" class="unavailable-search mono" style="min-width:210px;padding:0.28rem 0.55rem;font-size:0.82rem;" placeholder="Inspect package (e.g. emacs)..." aria-label="Inspect another package">
			<button type="submit" class="btn" style="padding:0.28rem 0.65rem;font-size:0.8rem;">View</button>
		</form>
	</div>`, querySuffix)

	if !st.TrackedInGuix && !st.TrackedInNonguix {
		b.WriteString(renderUntrackedPackagePanel(st))
	} else {
		b.WriteString(renderPackageSummaryPanel(st))
		b.WriteString(renderPackageEvaluationsTable(st))
	}
	b.WriteString(`</main>`)

	b.WriteString(renderFooter(dashboardState{
		ServerURL:    st.ServerURL,
		LastSyncedAt: st.LastSyncedAt,
	}))
	b.WriteString(renderCommonScripts())
	return b.String()
}

func renderPackagePageHeader(st packagePageState) string {
	querySuffix := html.EscapeString(buildQuerySuffix(st.ActiveSystem, st.RequireNonguix))
	var options strings.Builder
	for _, sys := range st.Systems {
		sel := ""
		if sys == st.ActiveSystem {
			sel = " selected"
		}
		escSys := html.EscapeString(sys)
		fmt.Fprintf(&options, `<option value="%s"%s>%s</option>`, escSys, sel, escSys)
	}

	checkedAttr := ""
	if st.RequireNonguix {
		checkedAttr = " checked"
	}

	return fmt.Sprintf(`
	<header class="site-header-container">
		<strong class="site-header-element-left"><a href="/%s">Guix Pull Ready</a></strong>
		<div class="site-header-element-right">
			<label style="font-size:0.88rem;font-weight:500;color:var(--muted);cursor:pointer;display:inline-flex;align-items:center;gap:0.35rem;"><input type="checkbox" id="main-nonguix-toggle"%s onchange="togglePackageNonguix(this.checked)"> Require <code>nonguix</code> channel</label>
			<select id="arch-select" class="arch-select mono" aria-label="Target architecture" onchange="changeSystem(this.value)">%s</select>
		</div>
	</header>
`, querySuffix, checkedAttr, options.String())
}

func renderUntrackedPackagePanel(st packagePageState) string {
	escSpec := html.EscapeString(st.RawSpec)
	escBase := html.EscapeString(st.BaseName)
	escSys := html.EscapeString(st.ActiveSystem)
	return fmt.Sprintf(`
	<div class="panel">
		<h2>Package <code>%s</code> Not Tracked on CI</h2>
		<p style="margin:0 0 0.75rem;color:var(--muted);font-size:0.94rem;">
			No build jobs matching <code>%s.%s</code> were found in recent cached evaluations on <code>ci.guix.gnu.org</code> or <code>cuirass.nonguix.org</code>.
		</p>
		<p style="margin:0;color:var(--muted);font-size:0.88rem;">
			This package may come from a local or third-party channel, may not be supported on <code>%s</code>, or the package name may be spelled differently.
		</p>
	</div>
`, escSpec, escBase, escSys, escSys)
}

func renderPackageSummaryPanel(st packagePageState) string {
	var b strings.Builder
	escSpec := html.EscapeString(st.RawSpec)
	escSys := html.EscapeString(st.ActiveSystem)

	var channelBadges strings.Builder
	if st.TrackedInGuix {
		channelBadges.WriteString(`<span class="badge badge-muted mono">guix</span>`)
	}
	if st.TrackedInNonguix {
		channelBadges.WriteString(`<span class="badge badge-muted mono">nonguix</span>`)
	}
	fmt.Fprintf(&channelBadges, `<span class="badge badge-muted mono">%s</span>`, escSys)

	fmt.Fprintf(&b, `
	<div class="panel">
		<div style="display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:0.5rem;margin-bottom:0.85rem;">
			<h2 class="mono" style="margin:0;font-size:1.25rem;">%s</h2>
			<div style="display:inline-flex;align-items:center;gap:0.4rem;">%s</div>
		</div>
`, escSpec, channelBadges.String())

	// Status cards grid
	b.WriteString(`<div class="pkg-summary-grid">`)

	// Card 1: Status in Latest Ready Commit
	b.WriteString(`<div class="pkg-summary-card">`)
	b.WriteString(`<div class="pkg-summary-label">Status in Latest Ready Commit</div>`)
	if st.LatestWholeReady == nil {
		b.WriteString(`<div style="font-size:0.9rem;color:var(--muted);">No whole-evaluation ready commit found in current window.</div>`)
	} else {
		r := st.LatestWholeReady
		short := html.EscapeString(shortCommit(r.Commit))
		escCommit := html.EscapeString(r.Commit)
		evalURL := html.EscapeString(evaluationHref(st.ServerURL, r.EvaluationID, st.ActiveSystem))
		fmt.Fprintf(&b, `<div style="display:flex;align-items:center;gap:0.5rem;flex-wrap:wrap;margin-top:0.2rem;">
			%s
			<span class="mono" style="font-size:0.86rem;">guix <a href="https://codeberg.org/guix/guix/commit/%s" target="_blank" rel="noopener">%s</a> (<a href="%s">#%d</a>)</span>
		</div>`,
			renderStatusBadgeHTML(r.Status),
			escCommit, short,
			evalURL, r.EvaluationID,
		)
		if r.Status == "succeeded" {
			b.WriteString(`<div style="margin-top:0.35rem;font-size:0.82rem;color:var(--muted);">Prebuilt substitute is ready in the default Latest Ready commit.</div>`)
		} else {
			fmt.Fprintf(&b, `<div style="margin-top:0.35rem;font-size:0.82rem;color:var(--muted);">Substitute is unavailable in evaluation #%d (<a href="%s" target="_blank" rel="noopener">inspect on Cuirass &rarr;</a>).</div>`,
				r.EvaluationID, html.EscapeString(r.InspectURL))
		}
	}
	b.WriteString(`</div>`)

	// Card 2: Newest Ready Commit for This Package
	b.WriteString(`<div class="pkg-summary-card">`)
	fmt.Fprintf(&b, `<div class="pkg-summary-label">Newest Ready Commit for <code>%s</code></div>`, escSpec)
	if st.LatestPkgReady == nil {
		b.WriteString(`<div style="font-size:0.9rem;color:var(--muted);">No evaluation in the last 40 had a succeeded substitute with <code>guix pull</code> ready.</div>`)
	} else {
		r := st.LatestPkgReady
		short := html.EscapeString(shortCommit(r.Commit))
		escCommit := html.EscapeString(r.Commit)
		evalURL := html.EscapeString(evaluationHref(st.ServerURL, r.EvaluationID, st.ActiveSystem))
		behindNote := "Latest Ready commit"
		if !r.IsLatestWholeReady {
			behindNote = fmt.Sprintf("%d evals from head", r.EvaluationsBehind)
		}
		fmt.Fprintf(&b, `<div style="display:flex;align-items:center;gap:0.5rem;flex-wrap:wrap;margin-top:0.2rem;">
			<span class="badge badge-ok">Ready</span>
			<span class="mono" style="font-size:0.86rem;">guix <a href="https://codeberg.org/guix/guix/commit/%s" target="_blank" rel="noopener">%s</a> (<a href="%s">#%d</a>, %s)</span>
		</div>`,
			escCommit, short,
			evalURL, r.EvaluationID,
			html.EscapeString(behindNote),
		)

		// If the package failed in the Latest Ready commit (or no whole-eval ready exists), show copyable fallback commands
		if st.LatestWholeReady == nil || st.LatestWholeReady.EvaluationID != r.EvaluationID {
			if st.PrimaryChannel == "guix" {
				tmCmd := fmt.Sprintf("guix time-machine --commit=%s -- install %s", r.Commit, st.RawSpec)
				escTmCmd := html.EscapeString(tmCmd)
				fmt.Fprintf(&b, `
				<div style="margin-top:0.5rem;">
					<div style="font-size:0.78rem;color:var(--muted);margin-bottom:0.25rem;">Install prebuilt binary from this commit via <code>guix time-machine</code>:</div>
					<div class="code-box-row">
						<pre class="mono" style="font-size:0.78rem;padding:0.35rem 0.55rem;">%s</pre>
						<button type="button" class="btn" style="padding:0.25rem 0.55rem;font-size:0.75rem;" data-copy="%s" onclick="copyFromBtn(this)">Copy</button>
					</div>
				</div>`, escTmCmd, escTmCmd)
			}
		} else {
			b.WriteString(`<div style="margin-top:0.35rem;font-size:0.82rem;color:var(--muted);">Matches the default Latest Ready commit.</div>`)
		}
	}
	b.WriteString(`</div>`)
	b.WriteString(`</div>`) // end .pkg-summary-grid

	// Health strip across last N evaluations
	if len(st.Evaluations) > 0 {
		stuckSummary := ""
		if st.StuckCount > 0 {
			stuckSummary = fmt.Sprintf(` &middot; <strong>%d</strong> Stuck`, st.StuckCount)
		}
		fmt.Fprintf(&b, `
		<div style="margin-top:0.9rem;">
			<div style="display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:0.5rem;font-size:0.82rem;color:var(--muted);margin-bottom:0.35rem;">
				<span>Last %d evaluations (newest &rarr; oldest):</span>
				<span><strong>%d</strong> Succeeded &middot; <strong>%d</strong> Failed &middot; <strong>%d</strong> Pending%s &middot; <strong>%d</strong> Not built</span>
			</div>
			<div class="health-strip">`,
			len(st.Evaluations),
			st.SucceededCount, st.FailedCount, st.PendingCount, stuckSummary, st.MissingCount,
		)
		for _, r := range st.Evaluations {
			cellClass := "health-cell " + healthCellClass(r.Status)
			tip := fmt.Sprintf("#%d (%s): %s", r.EvaluationID, shortCommit(r.Commit), r.Status)
			fmt.Fprintf(&b, `<a href="#eval-row-%d" class="%s" title="%s"></a>`,
				r.EvaluationID, cellClass, html.EscapeString(tip))
		}
		b.WriteString(`</div></div>`)
	}

	b.WriteString(`</div>`)
	return b.String()
}

func healthCellClass(status string) string {
	switch status {
	case "succeeded":
		return "health-ok"
	case "scheduled", "submitted", "started", "stuck":
		return "health-warn"
	case "not-built", "canceled":
		return "health-muted"
	default:
		return "health-danger"
	}
}

func renderPackageEvaluationsTable(st packagePageState) string {
	var b strings.Builder
	escSpec := html.EscapeString(st.RawSpec)
	showNonguixCol := st.RequireNonguix || st.PrimaryChannel == "nonguix"
	ngColStyle := ` style="display:none;"`
	if showNonguixCol {
		ngColStyle = ""
	}

	fmt.Fprintf(&b, `
	<div class="panel">
		<h2>Evaluation History for <code>%s</code></h2>
		<p class="sub">Showing %d cached evaluations on <code>%s</code> ordered from newest to oldest.</p>
`, escSpec, len(st.Evaluations), html.EscapeString(st.ActiveSystem))

	if len(st.Evaluations) == 0 {
		b.WriteString(`<p>No evaluations cached yet. Background sync is running...</p></div>`)
		return b.String()
	}

	fmt.Fprintf(&b, `
		<div class="table-wrap">
			<table>
				<thead>
					<tr>
						<th><a href="https://codeberg.org/guix/guix/commits/branch/master" target="_blank" rel="noopener">guix commit</a></th>
						<th>Committed</th>
						<th><a href="https://ci.guix.gnu.org/jobset/master" target="_blank" rel="noopener">guix/master</a></th>
						<th><a href="https://ci.guix.gnu.org/jobset/guix" target="_blank" rel="noopener">guix/guix</a></th>
						<th class="col-nonguix"%s><a href="https://cuirass.nonguix.org/jobset/nonguix" target="_blank" rel="noopener">nonguix/nonguix</a></th>
						<th><code>%s</code> Status</th>
						<th>Inspect</th>
					</tr>
				</thead>
				<tbody>
`, ngColStyle, escSpec)

	for _, r := range st.Evaluations {
		short := shortCommit(r.Commit)
		var rowAttrs strings.Builder
		fmt.Fprintf(&rowAttrs, ` id="eval-row-%d"`, r.EvaluationID)
		if r.IsLatestWholeReady || r.IsLatestPkgReady {
			rowAttrs.WriteString(` class="row-latest-ready"`)
		}

		var badges strings.Builder
		if r.IsLatestWholeReady {
			badges.WriteString(`<span class="badge badge-ok" style="margin-left:0.35rem;">Latest Ready</span>`)
		}
		if r.IsLatestPkgReady && !r.IsLatestWholeReady {
			badges.WriteString(`<span class="badge badge-ok" style="margin-left:0.35rem;">Newest Pkg Ready</span>`)
		}

		dateCell := `<td class="mono" style="white-space:nowrap;">unknown</td>`
		if !r.Timestamp.IsZero() {
			utcStr := r.Timestamp.UTC().Format("2006-01-02 15:04:05 UTC")
			shortUTC := r.Timestamp.UTC().Format("2006-01-02 15:04")
			isoStr := r.Timestamp.UTC().Format(time.RFC3339)
			dateCell = fmt.Sprintf(`<td class="mono local-time" style="white-space:nowrap;" data-utc="%s" title="%s">%s</td>`,
				html.EscapeString(isoStr), html.EscapeString(utcStr), html.EscapeString(shortUTC))
		}

		evalURL := html.EscapeString(evaluationHref(st.ServerURL, r.EvaluationID, st.ActiveSystem))
		readyURL := html.EscapeString(evaluationHrefWithStatus(st.ServerURL, r.EvaluationID, st.ActiveSystem, "succeeded"))
		masterBadge := fmt.Sprintf(`<a href="%s" class="badge badge-ok" title="All non-stuck builds completed in evaluation #%d (click to inspect available substitutes)">Ready</a>`,
			readyURL, r.EvaluationID)
		if r.EvalPendingJobs > 0 {
			pendingURL := html.EscapeString(evaluationHrefWithStatus(st.ServerURL, r.EvaluationID, st.ActiveSystem, "pending"))
			masterBadge = fmt.Sprintf(`<a href="%s" class="badge badge-warn" title="%d jobs pending in evaluation #%d (click to inspect)">%d Pending</a>`,
				pendingURL, r.EvalPendingJobs, r.EvaluationID, r.EvalPendingJobs)
		}
		var masterFailedBadge string
		if r.EvalFailedJobs > 0 {
			failedURL := html.EscapeString(evaluationHrefWithStatus(st.ServerURL, r.EvaluationID, st.ActiveSystem, "failed"))
			masterFailedBadge = fmt.Sprintf(`<a href="%s" class="badge badge-danger" title="%d jobs failed or had a failed dependency in evaluation #%d (click to inspect)">%d Failed</a>`,
				failedURL, r.EvalFailedJobs, r.EvaluationID, r.EvalFailedJobs)
		}
		masterCell := fmt.Sprintf(`<div style="display:inline-flex;align-items:center;gap:0.4rem;flex-wrap:wrap;"><a href="%s" class="mono"><strong>#%d</strong></a>%s%s</div>`,
			evalURL, r.EvaluationID, masterBadge, masterFailedBadge)

		pullCell := renderPullCell(r.PullEvalID, r.PullReady, r.PullPendingJobs, r.PullFailedJobs, st.ServerURL, st.ActiveSystem)

		evalItem := evaluationSummaryItem{
			EvaluationID: r.EvaluationID,
			Commit:       r.Commit,
			Timestamp:    r.Timestamp,
			PendingJobs:  r.EvalPendingJobs,
			FailedJobs:   r.EvalFailedJobs,
			PullReady:    r.PullReady,
			PullEvalID:   r.PullEvalID,
		}
		if r.HasNonguixEval {
			evalItem.AdditionalChannels = map[string]channelEvalSummary{"nonguix": r.NonguixEval}
		}
		nonguixCell := renderChannelCell("nonguix", evalItem, st.ActiveSystem, false)

		pkgStatusCell := renderPackageRowStatusCell(r)

		inspectLabel := "Cuirass builds &rarr;"
		if r.PrimaryBuildID > 0 {
			inspectLabel = fmt.Sprintf("Build #%d &rarr;", r.PrimaryBuildID)
		}
		inspectCell := fmt.Sprintf(`<a href="%s" target="_blank" rel="noopener" class="mono" style="font-size:0.8rem;">%s</a>`,
			html.EscapeString(r.InspectURL), inspectLabel)

		fmt.Fprintf(&b, `
					<tr%s>
						<td class="mono">
							<a href="https://codeberg.org/guix/guix/commit/%s" target="_blank" rel="noopener">%s</a>
							<button type="button" data-copy="%s" onclick="copyFromBtn(this)" style="padding:0.1rem 0.4rem;font-size:0.72rem;margin-left:0.3rem;">Copy</button>%s
						</td>
						%s
						<td>%s</td>
						<td>%s</td>
						<td class="col-nonguix"%s>%s</td>
						<td>%s</td>
						<td>%s</td>
					</tr>`,
			rowAttrs.String(),
			html.EscapeString(r.Commit), html.EscapeString(short), html.EscapeString(r.Commit), badges.String(),
			dateCell,
			masterCell,
			pullCell,
			ngColStyle,
			nonguixCell,
			pkgStatusCell,
			inspectCell,
		)
	}

	b.WriteString(`
				</tbody>
			</table>
		</div>
	</div>`)
	return b.String()
}

func renderPackageRowStatusCell(r packageEvalRow) string {
	if len(r.Jobs) <= 1 {
		if len(r.Jobs) == 1 && r.Jobs[0].Version != "" {
			return fmt.Sprintf(`<div style="display:inline-flex;align-items:center;gap:0.35rem;"><span class="mono" style="font-size:0.8rem;color:var(--muted);">@%s</span>%s</div>`,
				html.EscapeString(r.Jobs[0].Version), renderStatusBadgeHTML(r.Status))
		}
		return renderStatusBadgeHTML(r.Status)
	}

	var b strings.Builder
	b.WriteString(`<div style="display:flex;flex-wrap:wrap;gap:0.35rem;align-items:center;">`)
	for _, j := range r.Jobs {
		verLabel := j.Version
		if verLabel == "" {
			verLabel = j.JobName
		} else {
			verLabel = "@" + verLabel
		}
		fmt.Fprintf(&b, `<span style="display:inline-flex;align-items:center;gap:0.2rem;"><span class="mono" style="font-size:0.76rem;color:var(--muted);">%s</span>%s</span>`,
			html.EscapeString(verLabel), renderStatusBadgeHTML(j.Status.String()))
	}
	b.WriteString(`</div>`)
	return b.String()
}
