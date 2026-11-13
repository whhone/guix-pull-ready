package api

import (
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"

	"guix-pull-ready/internal/cuirass"
	"guix-pull-ready/internal/resolver"
	"guix-pull-ready/internal/store"
)

type evaluationPageState struct {
	BaseURL               string
	ServerURL             string
	NonguixServerURL      string
	ActiveServerURL       string
	Channel               string
	Jobset                string
	ServerName            string
	ActiveSystem          string
	Systems               []string
	RequireNonguix        bool
	LastSyncedAt          time.Time
	EvalID                int64
	Found                 bool
	Commit                string
	Timestamp             time.Time
	TotalJobs             int
	SucceededJobs         int
	FailedJobs            int
	PendingJobs           int
	StuckJobs             int
	NotBuiltCount         int
	PullReady             bool
	PullEvalID            int64
	PullPendingJobs       int
	PullFailedJobs        int
	HasNonguixEval        bool
	NonguixEval           channelEvalSummary
	PairedGuixCommit      string
	PairedGuixEvalID      int64
	PairedGuixFound       bool
	PairedGuixPendingJobs int
	PairedGuixFailedJobs  int
	IsLatestReady         bool
	AllItems              []unavailablePackageItem
	DetailedItems         []unavailablePackageItem
	SingleSucceeded       []string
	AvailableCount        int
	UnavailableCount      int
}

func buildEvaluationPageState(baseURL string, snap *store.Snapshot, channel string, serverName string, evalID int64, reqSystem string, requireNonguix bool) evaluationPageState {
	systems, activeSys := selectSystemsAndActive(snap, reqSystem)
	if channel == "" {
		channel = "guix"
	}
	st := evaluationPageState{
		BaseURL:          baseURL,
		ServerURL:        "https://ci.guix.gnu.org",
		NonguixServerURL: cuirass.DefaultChannelServerURL("nonguix"),
		Channel:          channel,
		ServerName:       serverName,
		ActiveSystem:     activeSys,
		Systems:          systems,
		RequireNonguix:   requireNonguix || channel == "nonguix",
		EvalID:           evalID,
	}

	if snap != nil {
		if snap.ServerURL != "" {
			st.ServerURL = snap.ServerURL
		}
		if ngURL := snap.ChannelServerURLs["nonguix"]; ngURL != "" {
			st.NonguixServerURL = ngURL
		}
		st.LastSyncedAt = snap.LastSyncedAt
	}

	if channel == "nonguix" {
		st.ActiveServerURL = st.NonguixServerURL
	} else {
		st.ActiveServerURL = st.ServerURL
	}
	if st.ServerName == "" {
		st.ServerName = serverHost(st.ActiveServerURL)
	}

	if snap == nil {
		return st
	}

	limit := 40
	if channel == "nonguix" {
		var targetEv store.EvaluationRecord
		var foundEv bool
		for _, ngEv := range snap.ChannelEvaluations["nonguix"] {
			if ngEv.ID == evalID {
				targetEv = ngEv
				foundEv = true
				break
			}
		}
		if !foundEv {
			return st
		}

		data, ok := snap.ByChannelEvalSystem[store.ChannelEvalSystemKey{
			Channel: "nonguix",
			EvalID:  targetEv.ID,
			System:  activeSys,
		}]
		if !ok || data == nil || data.TotalJobs == 0 {
			return st
		}

		st.Found = true
		st.Jobset = "nonguix/nonguix"
		st.Commit = targetEv.Commit
		st.Timestamp = targetEv.Timestamp
		st.TotalJobs = data.TotalJobs
		st.SucceededJobs = data.SucceededJobs
		st.FailedJobs = data.FailedJobs
		st.PendingJobs = data.PendingJobs
		st.StuckJobs = data.StuckJobs
		st.PairedGuixCommit = targetEv.Checkouts["guix"]

		if st.PairedGuixCommit != "" {
			if guixEv, ok := snap.GuixEvalByCommit[st.PairedGuixCommit]; ok {
				st.PairedGuixEvalID = guixEv.ID
				if guixData, ok := snap.ByEvalSystem[store.EvalSystemKey{EvalID: guixEv.ID, System: activeSys}]; ok && guixData != nil && guixData.TotalJobs > 0 {
					st.PairedGuixFound = true
					st.PairedGuixPendingJobs = guixData.PendingJobs
					st.PairedGuixFailedJobs = guixData.FailedJobs
					st.PullReady = guixData.PullReady
					st.PullEvalID = guixData.PullEvalID
					st.PullPendingJobs, st.PullFailedJobs = lookupPullJobCounts(snap, guixData.PullEvalID, activeSys)
				}
			}
		}

		if ngStrict, err := resolver.Resolve(snap, resolver.Query{
			System:             activeSys,
			MaxPending:         0,
			Limit:              limit,
			AdditionalChannels: []string{"nonguix"},
		}); err == nil && ngStrict != nil {
			for _, chRes := range ngStrict.AdditionalChannels {
				if chRes.Channel == "nonguix" && chRes.EvaluationID == targetEv.ID {
					st.IsLatestReady = true
					break
				}
			}
		}

		allItems, detailedItems, singleSucceeded := collectEvaluationPackages(snap, "nonguix", targetEv.ID, activeSys)
		accumulateEvalPackageCounts(&st, allItems)
		st.AllItems = allItems
		st.DetailedItems = detailedItems
		st.SingleSucceeded = singleSucceeded
		return st
	}

	// Default: "guix" server (ci.guix.gnu.org) - check guix/master first, then guix/guix.
	var targetEv store.EvaluationRecord
	var foundEv bool
	for _, ev := range snap.Evaluations {
		if ev.ID == evalID {
			targetEv = ev
			foundEv = true
			break
		}
	}
	if !foundEv {
		buildGuixPullEvaluationPageState(&st, snap, evalID, activeSys, limit)
		return st
	}

	data, ok := snap.ByEvalSystem[store.EvalSystemKey{EvalID: targetEv.ID, System: activeSys}]
	if !ok || data == nil || data.TotalJobs == 0 {
		return st
	}

	st.Found = true
	st.Jobset = "guix/master"
	st.Commit = targetEv.Commit
	st.Timestamp = targetEv.Timestamp
	st.TotalJobs = data.TotalJobs
	st.SucceededJobs = data.SucceededJobs
	st.FailedJobs = data.FailedJobs
	st.PendingJobs = data.PendingJobs
	st.StuckJobs = data.StuckJobs
	st.PullReady = data.PullReady
	st.PullEvalID = data.PullEvalID
	st.PullPendingJobs, st.PullFailedJobs = lookupPullJobCounts(snap, data.PullEvalID, activeSys)

	if strict, err := resolver.Resolve(snap, resolver.Query{
		System:     activeSys,
		MaxPending: 0,
		Limit:      limit,
	}); err == nil && strict != nil && strict.EvaluationID == targetEv.ID {
		st.IsLatestReady = true
	}

	chSum, hasNg := matchChannelEvaluation(snap, "nonguix", targetEv.Commit, activeSys)
	st.HasNonguixEval = hasNg
	st.NonguixEval = chSum

	allItems, detailedItems, singleSucceeded := collectEvaluationPackages(snap, "guix", targetEv.ID, activeSys)
	accumulateEvalPackageCounts(&st, allItems)
	st.AllItems = allItems
	st.DetailedItems = detailedItems
	st.SingleSucceeded = singleSucceeded
	return st
}

func lookupPullJobCounts(snap *store.Snapshot, pullEvalID int64, sys string) (int, int) {
	if snap == nil || pullEvalID <= 0 {
		return 0, 0
	}
	if pullData, ok := snap.ByChannelEvalSystem[store.ChannelEvalSystemKey{
		Channel: store.GuixJobsetChannel,
		EvalID:  pullEvalID,
		System:  sys,
	}]; ok && pullData != nil {
		return pullData.PendingJobs, pullData.FailedJobs
	}
	return 0, 0
}

func buildGuixPullEvaluationPageState(st *evaluationPageState, snap *store.Snapshot, evalID int64, activeSys string, limit int) {
	var pullEv store.EvaluationRecord
	var foundPullEv bool
	for _, ev := range snap.ChannelEvaluations[store.GuixJobsetChannel] {
		if ev.ID == evalID {
			pullEv = ev
			foundPullEv = true
			break
		}
	}

	if foundPullEv {
		data, ok := snap.ByChannelEvalSystem[store.ChannelEvalSystemKey{
			Channel: store.GuixJobsetChannel,
			EvalID:  pullEv.ID,
			System:  activeSys,
		}]
		if ok && data != nil && data.TotalJobs > 0 {
			st.Found = true
			st.Jobset = "guix/guix"
			st.Commit = pullEv.Commit
			st.Timestamp = pullEv.Timestamp
			st.TotalJobs = data.TotalJobs
			st.SucceededJobs = data.SucceededJobs
			st.FailedJobs = data.FailedJobs
			st.PendingJobs = data.PendingJobs
			st.StuckJobs = data.StuckJobs
			st.PullEvalID = evalID
			st.PullReady = data.PendingJobs == 0 && data.SucceededJobs > 0 && data.FailedJobs == 0
			st.PullPendingJobs = data.PendingJobs
			st.PullFailedJobs = data.FailedJobs

			allItems, detailedItems, singleSucceeded := collectEvaluationPackages(snap, store.GuixJobsetChannel, evalID, activeSys)
			accumulateEvalPackageCounts(st, allItems)
			st.AllItems = allItems
			st.DetailedItems = detailedItems
			st.SingleSucceeded = singleSucceeded
		}
	}

	// Fallback for stores where SetPullStatus populated PullEvalID on guix/master without full guix/guix job rows.
	if !st.Found && cuirass.SupportsGuixJobset(activeSys) {
		for _, masterEv := range snap.Evaluations {
			mData, ok := snap.ByEvalSystem[store.EvalSystemKey{EvalID: masterEv.ID, System: activeSys}]
			if ok && mData != nil && mData.PullEvalID == evalID {
				st.Found = true
				st.Jobset = "guix/guix"
				st.Commit = masterEv.Commit
				st.Timestamp = masterEv.Timestamp
				st.PullEvalID = evalID
				st.PullReady = mData.PullReady
				st.TotalJobs = 1
				if mData.PullReady {
					st.SucceededJobs = 1
					item := unavailablePackageItem{Name: "guix", Channel: "guix", Status: "succeeded"}
					st.AllItems = []unavailablePackageItem{item}
					st.SingleSucceeded = []string{"guix"}
					st.AvailableCount = 1
				} else {
					st.PendingJobs = 1
					st.PullPendingJobs = 1
					item := unavailablePackageItem{Name: "guix", Channel: "guix", Status: "scheduled"}
					st.AllItems = []unavailablePackageItem{item}
					st.DetailedItems = []unavailablePackageItem{item}
					st.UnavailableCount = 1
				}
				break
			}
		}
	}

	if !st.Found {
		return
	}

	st.PairedGuixCommit = st.Commit
	if st.Commit != "" {
		if masterEv, ok := snap.GuixEvalByCommit[st.Commit]; ok {
			st.PairedGuixEvalID = masterEv.ID
			if st.Timestamp.IsZero() {
				st.Timestamp = masterEv.Timestamp
			}
			if mData, ok := snap.ByEvalSystem[store.EvalSystemKey{EvalID: masterEv.ID, System: activeSys}]; ok && mData != nil && mData.TotalJobs > 0 {
				st.PairedGuixFound = true
				st.PairedGuixPendingJobs = mData.PendingJobs
				st.PairedGuixFailedJobs = mData.FailedJobs
			}
		}
	}

	if strict, err := resolver.Resolve(snap, resolver.Query{
		System:     activeSys,
		MaxPending: 0,
		Limit:      limit,
	}); err == nil && strict != nil && strict.PullEvaluationID == evalID {
		st.IsLatestReady = true
	}

	chSum, hasNg := matchChannelEvaluation(snap, "nonguix", st.Commit, activeSys)
	st.HasNonguixEval = hasNg
	st.NonguixEval = chSum
}

func accumulateEvalPackageCounts(st *evaluationPageState, items []unavailablePackageItem) {
	for _, it := range items {
		if len(it.Builds) > 0 {
			for _, b := range it.Builds {
				if b.Status == "succeeded" {
					st.AvailableCount++
				} else {
					st.UnavailableCount++
				}
			}
			continue
		}
		if it.Status == "succeeded" {
			st.AvailableCount++
		} else {
			st.UnavailableCount++
			if it.Status == "not-built" {
				st.NotBuiltCount++
			}
		}
	}
}

func renderHTMLEvaluationPage(st evaluationPageState) string {
	var b strings.Builder
	title := fmt.Sprintf("Evaluation #%d (%s) - Guix Pull Ready", st.EvalID, st.ServerName)
	b.WriteString(renderPageHead(title))
	b.WriteString(renderEvaluationPageHeader(st))

	b.WriteString(`<main>`)
	querySuffix := html.EscapeString(buildQuerySuffix(st.ActiveSystem, st.RequireNonguix))
	fmt.Fprintf(&b, `<div style="margin-bottom:0.85rem;font-size:0.88rem;display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:0.5rem;">
		<a href="/%s">&larr; Back to Dashboard</a>
		<form onsubmit="return jumpToPackage(this)" style="display:inline-flex;align-items:center;gap:0.4rem;">
			<input type="search" name="pkg" class="unavailable-search mono" style="min-width:210px;padding:0.28rem 0.55rem;font-size:0.82rem;" placeholder="Inspect package (e.g. emacs)..." aria-label="Inspect a package">
			<button type="submit" class="btn" style="padding:0.28rem 0.65rem;font-size:0.8rem;">View</button>
		</form>
	</div>`, querySuffix)

	if !st.Found {
		b.WriteString(renderUncachedEvaluationPanel(st))
	} else {
		b.WriteString(renderEvaluationOverviewPanel(st))
		b.WriteString(renderEvaluationPackagesPanel(st))
	}
	b.WriteString(`</main>`)

	b.WriteString(renderFooter(dashboardState{
		ServerURL:    st.ActiveServerURL,
		LastSyncedAt: st.LastSyncedAt,
	}))
	b.WriteString(renderCommonScripts())
	return b.String()
}

func renderEvaluationPageHeader(st evaluationPageState) string {
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
	escServerName := html.EscapeString(st.ServerName)
	return fmt.Sprintf(`
	<header class="site-header-container">
		<strong class="site-header-element-left"><a href="/%s">Guix Pull Ready</a></strong>
		<div class="site-header-element-right">
			<span class="badge badge-muted mono">%s</span>
			<select id="arch-select" class="arch-select mono" aria-label="Target architecture" onchange="changeSystem(this.value)">%s</select>
		</div>
	</header>
`, querySuffix, escServerName, options.String())
}

func renderUncachedEvaluationPanel(st evaluationPageState) string {
	escServerURL := html.EscapeString(st.ActiveServerURL)
	escServerName := html.EscapeString(st.ServerName)
	escSys := html.EscapeString(st.ActiveSystem)
	return fmt.Sprintf(`
	<div class="panel">
		<h2>Evaluation <code>#%d</code> Not Found in Cache</h2>
		<p style="margin:0 0 0.75rem;color:var(--muted);font-size:0.94rem;">
			Evaluation <code>#%d</code> on <code>%s</code> is not in the recent cached evaluations window for <code>%s</code>.
		</p>
		<p style="margin:0;font-size:0.9rem;">
			<a href="%s/eval/%d" target="_blank" rel="noopener" class="mono">View Evaluation #%d on Cuirass &rarr;</a>
		</p>
	</div>
`, st.EvalID, st.EvalID, escServerName, escSys, escServerURL, st.EvalID, st.EvalID)
}

func renderEvaluationOverviewPanel(st evaluationPageState) string {
	var b strings.Builder
	escActiveServerURL := html.EscapeString(st.ActiveServerURL)
	escServerName := html.EscapeString(st.ServerName)
	escSys := html.EscapeString(st.ActiveSystem)
	escCommit := html.EscapeString(st.Commit)
	short := html.EscapeString(shortCommit(st.Commit))

	succeededURL := html.EscapeString(evaluationHrefWithStatus(st.ActiveServerURL, st.EvalID, st.ActiveSystem, "succeeded"))
	failedURL := html.EscapeString(evaluationHrefWithStatus(st.ActiveServerURL, st.EvalID, st.ActiveSystem, "failed"))
	pendingURL := html.EscapeString(evaluationHrefWithStatus(st.ActiveServerURL, st.EvalID, st.ActiveSystem, "pending"))
	notBuiltURL := html.EscapeString(evaluationHrefWithStatus(st.ActiveServerURL, st.EvalID, st.ActiveSystem, "not-built"))

	var badges strings.Builder
	if st.IsLatestReady {
		badges.WriteString(`<span class="badge badge-ok">Latest Ready</span>`)
	}
	if st.PendingJobs == 0 {
		if st.Jobset == "guix/guix" && !st.PullReady && st.FailedJobs > 0 {
			fmt.Fprintf(&badges, `<a href="%s" onclick="return setUnavailableStatusFilter(event, 'failed')" class="badge badge-danger">%d Failed</a>`, failedURL, st.FailedJobs)
		} else {
			fmt.Fprintf(&badges, `<a href="%s" onclick="return setUnavailableStatusFilter(event, 'succeeded')" class="badge badge-ok">Ready</a>`, succeededURL)
		}
	} else {
		fmt.Fprintf(&badges, `<a href="%s" onclick="return setUnavailableStatusFilter(event, 'pending')" class="badge badge-warn">%d Pending</a>`, pendingURL, st.PendingJobs)
	}
	var stuckBreakdownBadge string
	if st.StuckJobs > 0 {
		stuckURL := html.EscapeString(evaluationHrefWithStatus(st.ActiveServerURL, st.EvalID, st.ActiveSystem, "stuck"))
		stuckBadge := fmt.Sprintf(`<a href="%s" onclick="return setUnavailableStatusFilter(event, 'stuck')" class="badge badge-warn">%d Stuck</a>`, stuckURL, st.StuckJobs)
		badges.WriteString(stuckBadge)
		stuckBreakdownBadge = "\n\t\t\t" + stuckBadge
	}
	if st.Jobset == "guix/guix" {
		badges.WriteString(`<span class="badge badge-muted mono">guix/guix</span>`)
	}
	fmt.Fprintf(&badges, `<span class="badge badge-muted mono">%s</span>`, escServerName)
	fmt.Fprintf(&badges, `<span class="badge badge-muted mono">%s</span>`, escSys)

	fmt.Fprintf(&b, `
	<div class="panel">
		<div style="display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:0.5rem;margin-bottom:0.85rem;">
			<div style="display:inline-flex;align-items:center;gap:0.5rem;flex-wrap:wrap;">
				<h2 class="mono" style="margin:0;font-size:1.25rem;">Evaluation #%d</h2>
				%s
			</div>
			<a href="%s/eval/%d" target="_blank" rel="noopener" class="mono" style="font-size:0.86rem;">Cuirass #%d &rarr;</a>
		</div>
`, st.EvalID, badges.String(), escActiveServerURL, st.EvalID, st.EvalID)

	b.WriteString(`<div class="pkg-summary-grid">`)

	if st.Channel == "nonguix" {
		// Card 1: Nonguix Commit & Timestamp
		b.WriteString(`<div class="pkg-summary-card">`)
		b.WriteString(`<div class="pkg-summary-label">Nonguix Commit &amp; Timestamp</div>`)
		fmt.Fprintf(&b, `<div class="mono" style="font-size:0.9rem;">
			<a href="https://gitlab.com/nonguix/nonguix/-/commit/%s" target="_blank" rel="noopener">%s</a>
			<button type="button" data-copy="%s" onclick="copyFromBtn(this)" style="padding:0.1rem 0.4rem;font-size:0.72rem;margin-left:0.3rem;">Copy</button>
		</div>`, escCommit, short, escCommit)
		if !st.Timestamp.IsZero() {
			utcStr := st.Timestamp.UTC().Format("2006-01-02 15:04:05 UTC")
			isoStr := st.Timestamp.UTC().Format(time.RFC3339)
			fmt.Fprintf(&b, `<div class="mono local-time" data-utc="%s" data-show-sec="1" title="%s" style="margin-top:0.3rem;font-size:0.82rem;color:var(--muted);">%s</div>`,
				html.EscapeString(isoStr), html.EscapeString(utcStr), html.EscapeString(utcStr))
		}
		b.WriteString(`</div>`)

		// Card 2: nonguix/nonguix Job Breakdown
		b.WriteString(`<div class="pkg-summary-card">`)
		b.WriteString(`<div class="pkg-summary-label">nonguix/nonguix Job Breakdown</div>`)
		fmt.Fprintf(&b, `<div style="display:flex;align-items:center;gap:0.4rem;flex-wrap:wrap;">
			<a href="%s" onclick="return setUnavailableStatusFilter(event, 'succeeded')" class="badge badge-ok">%d Succeeded</a>
			<a href="%s" onclick="return setUnavailableStatusFilter(event, 'failed')" class="badge badge-danger">%d Failed</a>
			<a href="%s" onclick="return setUnavailableStatusFilter(event, 'pending')" class="badge badge-warn">%d Pending</a>%s
			<a href="%s" onclick="return setUnavailableStatusFilter(event, 'not-built')" class="badge badge-muted">%d Not built</a>
		</div>
		<div style="margin-top:0.35rem;font-size:0.8rem;color:var(--muted);">Total jobs in evaluation: %d</div>`,
			succeededURL, st.SucceededJobs, failedURL, st.FailedJobs, pendingURL, st.PendingJobs, stuckBreakdownBadge, notBuiltURL, st.NotBuiltCount, st.TotalJobs)
		b.WriteString(`</div>`)

		// Card 3: Paired guix/master Evaluation
		b.WriteString(`<div class="pkg-summary-card">`)
		b.WriteString(`<div class="pkg-summary-label">Paired guix/master Evaluation</div>`)
		renderPairedGuixMasterCardBody(&b, st, true)
		b.WriteString(`</div>`)

		// Card 4: guix/guix Pull Jobset
		b.WriteString(`<div class="pkg-summary-card">`)
		b.WriteString(`<div class="pkg-summary-label">guix/guix Pull Jobset</div>`)
		renderPullJobsetCardBody(&b, st)
		b.WriteString(`</div>`)
	} else {
		// Card 1: Guix Commit & Timestamp
		b.WriteString(`<div class="pkg-summary-card">`)
		b.WriteString(`<div class="pkg-summary-label">Guix Commit &amp; Timestamp</div>`)
		fmt.Fprintf(&b, `<div class="mono" style="font-size:0.9rem;">
			<a href="https://codeberg.org/guix/guix/commit/%s" target="_blank" rel="noopener">%s</a>
			<button type="button" data-copy="%s" onclick="copyFromBtn(this)" style="padding:0.1rem 0.4rem;font-size:0.72rem;margin-left:0.3rem;">Copy</button>
		</div>`, escCommit, short, escCommit)
		if !st.Timestamp.IsZero() {
			utcStr := st.Timestamp.UTC().Format("2006-01-02 15:04:05 UTC")
			isoStr := st.Timestamp.UTC().Format(time.RFC3339)
			fmt.Fprintf(&b, `<div class="mono local-time" data-utc="%s" data-show-sec="1" title="%s" style="margin-top:0.3rem;font-size:0.82rem;color:var(--muted);">%s</div>`,
				html.EscapeString(isoStr), html.EscapeString(utcStr), html.EscapeString(utcStr))
		}
		b.WriteString(`</div>`)

		// Card 2: Job Breakdown (guix/master or guix/guix)
		breakdownLabel := "guix/master Job Breakdown"
		if st.Jobset == "guix/guix" {
			breakdownLabel = "guix/guix Job Breakdown"
		}
		b.WriteString(`<div class="pkg-summary-card">`)
		fmt.Fprintf(&b, `<div class="pkg-summary-label">%s</div>`, breakdownLabel)
		fmt.Fprintf(&b, `<div style="display:flex;align-items:center;gap:0.4rem;flex-wrap:wrap;">
			<a href="%s" onclick="return setUnavailableStatusFilter(event, 'succeeded')" class="badge badge-ok">%d Succeeded</a>
			<a href="%s" onclick="return setUnavailableStatusFilter(event, 'failed')" class="badge badge-danger">%d Failed</a>
			<a href="%s" onclick="return setUnavailableStatusFilter(event, 'pending')" class="badge badge-warn">%d Pending</a>%s
			<a href="%s" onclick="return setUnavailableStatusFilter(event, 'not-built')" class="badge badge-muted">%d Not built</a>
		</div>
		<div style="margin-top:0.35rem;font-size:0.8rem;color:var(--muted);">Total jobs in evaluation: %d</div>`,
			succeededURL, st.SucceededJobs, failedURL, st.FailedJobs, pendingURL, st.PendingJobs, stuckBreakdownBadge, notBuiltURL, st.NotBuiltCount, st.TotalJobs)
		b.WriteString(`</div>`)

		// Card 3: Paired guix/master Evaluation (when viewing guix/guix) or guix/guix Pull Jobset (when viewing guix/master)
		if st.Jobset == "guix/guix" {
			b.WriteString(`<div class="pkg-summary-card">`)
			b.WriteString(`<div class="pkg-summary-label">Paired guix/master Evaluation</div>`)
			renderPairedGuixMasterCardBody(&b, st, false)
			b.WriteString(`</div>`)
		} else {
			b.WriteString(`<div class="pkg-summary-card">`)
			b.WriteString(`<div class="pkg-summary-label">guix/guix Pull Jobset</div>`)
			renderPullJobsetCardBody(&b, st)
			b.WriteString(`</div>`)
		}

		// Card 4: Coordinated nonguix/nonguix Evaluation
		b.WriteString(`<div class="pkg-summary-card">`)
		b.WriteString(`<div class="pkg-summary-label">nonguix/nonguix Evaluation</div>`)
		evalItem := evaluationSummaryItem{
			EvaluationID: st.EvalID,
			Commit:       st.Commit,
			Timestamp:    st.Timestamp,
			PendingJobs:  st.PendingJobs,
			FailedJobs:   st.FailedJobs,
			PullReady:    st.PullReady,
			PullEvalID:   st.PullEvalID,
		}
		if st.HasNonguixEval {
			evalItem.AdditionalChannels = map[string]channelEvalSummary{"nonguix": st.NonguixEval}
		}
		b.WriteString(renderChannelCell("nonguix", evalItem, st.ActiveSystem, false))
		if st.HasNonguixEval && st.NonguixEval.Commit != "" {
			escNgCommit := html.EscapeString(st.NonguixEval.Commit)
			shortNg := html.EscapeString(shortCommit(st.NonguixEval.Commit))
			fmt.Fprintf(&b, `<div class="mono" style="margin-top:0.35rem;font-size:0.8rem;color:var(--muted);">commit <a href="https://gitlab.com/nonguix/nonguix/-/commit/%s" target="_blank" rel="noopener">%s</a></div>`,
				escNgCommit, shortNg)
		}
		b.WriteString(`</div>`)
	}

	b.WriteString(`</div>`) // end .pkg-summary-grid
	b.WriteString(`</div>`) // end .panel
	return b.String()
}

func renderPairedGuixMasterCardBody(b *strings.Builder, st evaluationPageState, showCommit bool) {
	if st.PairedGuixFound && st.PairedGuixEvalID > 0 {
		guixEvalURL := html.EscapeString(evaluationHref(st.ServerURL, st.PairedGuixEvalID, st.ActiveSystem))
		guixReadyURL := html.EscapeString(evaluationHrefWithStatus(st.ServerURL, st.PairedGuixEvalID, st.ActiveSystem, "succeeded"))
		guixBadge := fmt.Sprintf(`<a href="%s" class="badge badge-ok">Ready</a>`, guixReadyURL)
		if st.PairedGuixPendingJobs > 0 {
			guixPendingURL := html.EscapeString(evaluationHrefWithStatus(st.ServerURL, st.PairedGuixEvalID, st.ActiveSystem, "pending"))
			guixBadge = fmt.Sprintf(`<a href="%s" class="badge badge-warn">%d Pending</a>`,
				guixPendingURL, st.PairedGuixPendingJobs)
		}
		var guixFailedBadge string
		if st.PairedGuixFailedJobs > 0 {
			guixFailedURL := html.EscapeString(evaluationHrefWithStatus(st.ServerURL, st.PairedGuixEvalID, st.ActiveSystem, "failed"))
			guixFailedBadge = fmt.Sprintf(`<a href="%s" class="badge badge-danger">%d Failed</a>`,
				guixFailedURL, st.PairedGuixFailedJobs)
		}
		fmt.Fprintf(b, `<div style="display:inline-flex;align-items:center;gap:0.4rem;flex-wrap:wrap;">
			<a href="%s" class="mono"><strong>#%d</strong></a>%s%s
		</div>`, guixEvalURL, st.PairedGuixEvalID, guixBadge, guixFailedBadge)
	} else {
		b.WriteString(`<div><span style="color:var(--muted);font-size:0.85rem;">Not in cached window</span></div>`)
	}
	if showCommit && st.PairedGuixCommit != "" {
		escGuixCommit := html.EscapeString(st.PairedGuixCommit)
		shortGuix := html.EscapeString(shortCommit(st.PairedGuixCommit))
		fmt.Fprintf(b, `<div class="mono" style="margin-top:0.35rem;font-size:0.8rem;color:var(--muted);">guix commit <a href="https://codeberg.org/guix/guix/commit/%s" target="_blank" rel="noopener">%s</a></div>`,
			escGuixCommit, shortGuix)
	} else if !showCommit {
		b.WriteString(`<div style="margin-top:0.35rem;font-size:0.8rem;color:var(--muted);">Full package set evaluation for this commit.</div>`)
	}
}

func renderPullJobsetCardBody(b *strings.Builder, st evaluationPageState) {
	escSys := html.EscapeString(st.ActiveSystem)
	if !cuirass.SupportsGuixJobset(st.ActiveSystem) {
		fmt.Fprintf(b, `<div><span class="badge badge-muted">Not built</span></div>
		<div style="margin-top:0.35rem;font-size:0.8rem;color:var(--muted);">ci.guix.gnu.org does not build the guix jobset for %s.</div>`, escSys)
		return
	}
	fmt.Fprintf(b, `<div>%s</div>`,
		renderPullCell(st.PullEvalID, st.PullReady, st.PullPendingJobs, st.PullFailedJobs, st.ServerURL, st.ActiveSystem))
	b.WriteString(`<div style="margin-top:0.35rem;font-size:0.8rem;color:var(--muted);">Prebuilt binary status for <code>guix pull</code> itself.</div>`)
}

func renderEvaluationPackagesPanel(st evaluationPageState) string {
	detailed := st.DetailedItems
	if detailed == nil {
		detailed = []unavailablePackageItem{}
	}
	payload := unavailableModePayload{
		Available:        true,
		ServerURL:        st.ServerURL,
		NonguixURL:       st.NonguixServerURL,
		System:           st.ActiveSystem,
		AvailableCount:   st.AvailableCount,
		UnavailableCount: st.UnavailableCount,
		Items:            detailed,
	}
	var evalMetaText string
	channelLabel := st.Channel
	if st.Channel == "nonguix" {
		payload.NonguixCommit = st.Commit
		payload.NonguixEvalID = st.EvalID
		payload.NonguixSucceeded = st.SingleSucceeded
		evalMetaText = fmt.Sprintf("nonguix #%d (%s)", st.EvalID, shortCommit(st.Commit))
	} else if st.Jobset == "guix/guix" {
		payload.Spec = "guix"
		payload.Commit = st.Commit
		payload.EvaluationID = st.EvalID
		payload.Succeeded = st.SingleSucceeded
		evalMetaText = fmt.Sprintf("guix/guix #%d (%s)", st.EvalID, shortCommit(st.Commit))
		channelLabel = "guix/guix"
	} else {
		payload.Commit = st.Commit
		payload.EvaluationID = st.EvalID
		payload.Succeeded = st.SingleSucceeded
		evalMetaText = fmt.Sprintf("guix #%d (%s)", st.EvalID, shortCommit(st.Commit))
	}

	jsonBytes, _ := json.Marshal(payload)
	safeJSON := strings.ReplaceAll(string(jsonBytes), "</", `<\/`)

	availURL := html.EscapeString(evaluationHrefWithStatus(st.ActiveServerURL, st.EvalID, st.ActiveSystem, "succeeded"))
	unavailURL := html.EscapeString(evaluationHrefWithStatus(st.ActiveServerURL, st.EvalID, st.ActiveSystem, "unavailable"))
	availBadgeText := fmt.Sprintf("%d available", payload.AvailableCount)
	unavailBadgeClass := "badge badge-ok"
	unavailBadgeText := "0 unavailable"
	if payload.UnavailableCount > 0 {
		unavailBadgeClass = "badge badge-danger"
		unavailBadgeText = fmt.Sprintf("%d unavailable", payload.UnavailableCount)
	}
	totalPkgs := payload.AvailableCount + payload.UnavailableCount
	matchCountText := fmt.Sprintf("Total: %d packages (%d available, %d unavailable)",
		totalPkgs, payload.AvailableCount, payload.UnavailableCount)

	var tbody strings.Builder
	pageSize := 100
	totalItems := len(st.AllItems)
	totalPages := (totalItems + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}
	paginationStyle := ""
	pageRangeText := "Showing 0 of 0"
	nextDisabledAttr := " disabled"
	if totalItems == 0 {
		paginationStyle = ` style="display:none;"`
		tbody.WriteString(`<tr><td colspan="4" style="color:var(--muted);text-align:center;padding:1rem;">No packages tracked in this evaluation.</td></tr>`)
	} else {
		maxInitial := pageSize
		if totalItems < maxInitial {
			maxInitial = totalItems
		}
		pageRangeText = fmt.Sprintf("Showing 1-%d of %d", maxInitial, totalItems)
		if totalPages > 1 {
			nextDisabledAttr = ""
		}
		for i := 0; i < maxInitial; i++ {
			it := st.AllItems[i]
			ch := it.Channel
			if ch == "" {
				ch = st.Channel
			}
			querySuffix := html.EscapeString(buildQuerySuffix(st.ActiveSystem, st.RequireNonguix || ch == "nonguix"))
			pkgURL := "/packages/" + url.PathEscape(it.Name) + querySuffix
			tbody.WriteString(renderEvaluationPackageRowHTML(payload, it, ch, pkgURL))
		}
	}

	return fmt.Sprintf(`
	<div id="unavailable-panel" class="panel">
		<div style="display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:0.55rem;margin-bottom:0.5rem;">
			<div style="display:inline-flex;align-items:center;gap:0.55rem;flex-wrap:wrap;">
				<h2 style="margin:0;">Packages in Evaluation</h2>
				<a id="eval-available-badge" href="%s" onclick="return setUnavailableStatusFilter(event, 'succeeded')" class="badge badge-ok">%s</a>
				<a id="unavailable-total-badge" href="%s" onclick="return setUnavailableStatusFilter(event, 'unavailable')" class="%s">%s</a>
				<span id="unavailable-eval-meta" class="mono" style="font-size:0.8rem;color:var(--muted);">%s</span>
			</div>
		</div>
		<p class="sub" style="margin-bottom:0.65rem;">
			Showing all tracked <code>%s</code> packages in evaluation <code>#%d</code> on <code>%s</code> (both available substitutes and unavailable/pending builds). Click any package name to view its 40-evaluation history.
		</p>
		<div class="unavailable-controls">
			<input type="search" id="unavailable-search" class="unavailable-search mono" placeholder="Search package or comma-separated list (e.g. github-cli, emacs)..." aria-label="Search packages in evaluation" oninput="filterUnavailablePackages()">
			<select id="unavailable-status-filter" class="unavailable-filter" aria-label="Filter by package status" onchange="filterUnavailablePackages()">
				<option value="">All packages</option>
				<option value="succeeded">Available (succeeded)</option>
				<option value="unavailable">All unavailable</option>
				<option value="failed">All failed</option>
				<option value="failed-build">Failed build</option>
				<option value="failed-dependency">Failed dependency</option>
				<option value="pending">Pending</option>
				<option value="stuck">Stuck</option>
				<option value="not-built">Not built</option>
				<option value="canceled-other">Canceled / Other</option>
			</select>
			<span id="unavailable-match-count" style="font-size:0.8rem;color:var(--muted);">%s</span>
		</div>
		<div id="eval-table-wrap" class="table-wrap" style="max-height:520px;overflow-y:auto;">
			<table>
				<thead>
					<tr>
						<th>Package</th>
						<th>Channel</th>
						<th>Status</th>
						<th>Inspect on Cuirass</th>
					</tr>
				</thead>
				<tbody id="unavailable-tbody">%s</tbody>
			</table>
		</div>
		<div id="eval-pagination-wrap" class="eval-pagination"%s>
			<div class="eval-pagination-left">
				<span id="eval-page-range" class="mono" style="font-size:0.8rem;color:var(--muted);">%s</span>
				<select id="eval-page-size" class="unavailable-filter" style="padding:0.25rem 0.5rem;font-size:0.8rem;" aria-label="Rows per page" onchange="changeEvalPageSize(this.value)">
					<option value="100" selected>100 / page</option>
					<option value="250">250 / page</option>
					<option value="500">500 / page</option>
				</select>
			</div>
			<div class="eval-pagination-right">
				<button type="button" id="eval-page-first" class="btn eval-page-btn" onclick="goToEvalPage(1)" disabled title="First page">&laquo; First</button>
				<button type="button" id="eval-page-prev" class="btn eval-page-btn" onclick="stepEvalPage(-1)" disabled title="Previous page">&lsaquo; Prev</button>
				<span class="eval-page-jump">Page <input type="number" id="eval-page-input" class="eval-page-input mono" min="1" max="%d" value="1" aria-label="Page number" onchange="goToEvalPage(this.value)" onkeydown="if(event.key==='Enter'){event.preventDefault();goToEvalPage(this.value);}"> of <span id="eval-page-total" class="mono">%d</span></span>
				<button type="button" id="eval-page-next" class="btn eval-page-btn" onclick="stepEvalPage(1)"%s title="Next page">Next &rsaquo;</button>
				<button type="button" id="eval-page-last" class="btn eval-page-btn" onclick="goToEvalLastPage()"%s title="Last page">Last &raquo;</button>
			</div>
		</div>
		<script id="unavailable-pkgs-data" type="application/json">%s</script>
	</div>
`,
		availURL,
		html.EscapeString(availBadgeText),
		unavailURL,
		unavailBadgeClass,
		html.EscapeString(unavailBadgeText),
		html.EscapeString(evalMetaText),
		html.EscapeString(channelLabel),
		st.EvalID,
		html.EscapeString(st.ServerName),
		html.EscapeString(matchCountText),
		tbody.String(),
		paginationStyle,
		html.EscapeString(pageRangeText),
		totalPages,
		totalPages,
		nextDisabledAttr,
		nextDisabledAttr,
		safeJSON,
	)
}

func renderEvaluationPackageRowHTML(payload unavailableModePayload, item unavailablePackageItem, ch string, pkgURL string) string {
	escName := html.EscapeString(item.Name)
	escCh := html.EscapeString(ch)
	if len(item.Builds) > 1 {
		pkgCell := fmt.Sprintf(`<a href="%s">%s</a> <span class="badge badge-muted" style="margin-left:0.35rem;">%d builds</span>`,
			pkgURL, escName, len(item.Builds))
		var statusParts strings.Builder
		var inspectParts strings.Builder
		statusParts.WriteString(`<div style="display:flex;flex-direction:column;gap:0.28rem;">`)
		inspectParts.WriteString(`<div style="display:flex;flex-direction:column;gap:0.28rem;">`)
		for _, b := range item.Builds {
			verPrefix := ""
			if b.Version != "" {
				verPrefix = fmt.Sprintf(`<span class="mono" style="font-size:0.78rem;color:var(--muted);">@%s</span> `, html.EscapeString(b.Version))
			} else if b.BuildID > 0 {
				verPrefix = fmt.Sprintf(`<span class="mono" style="font-size:0.78rem;color:var(--muted);">#%d</span> `, b.BuildID)
			}
			fmt.Fprintf(&statusParts, `<div style="display:inline-flex;align-items:center;gap:0.35rem;">%s%s</div>`,
				verPrefix, renderStatusBadgeHTML(b.Status))
			bItem := unavailablePackageItem{
				Name:    item.Name,
				Channel: ch,
				Status:  b.Status,
				BuildID: b.BuildID,
			}
			inspectURL, inspectLabel := buildPackageInspectLinkServer(payload, bItem)
			fmt.Fprintf(&inspectParts, `<div><a href="%s" target="_blank" rel="noopener" class="mono" style="font-size:0.8rem;">%s</a></div>`,
				html.EscapeString(inspectURL), inspectLabel)
		}
		statusParts.WriteString(`</div>`)
		inspectParts.WriteString(`</div>`)
		return fmt.Sprintf(`<tr><td class="mono">%s</td><td class="mono"><span class="badge badge-muted">%s</span></td><td>%s</td><td>%s</td></tr>`,
			pkgCell, escCh, statusParts.String(), inspectParts.String())
	}

	inspectURL, inspectLabel := buildPackageInspectLinkServer(payload, item)
	statusCell := renderStatusBadgeHTML(item.Status)
	if len(item.Builds) == 1 && item.Builds[0].Version != "" {
		statusCell = fmt.Sprintf(`<div style="display:inline-flex;align-items:center;gap:0.35rem;"><span class="mono" style="font-size:0.78rem;color:var(--muted);">@%s</span>%s</div>`,
			html.EscapeString(item.Builds[0].Version), statusCell)
	}
	return fmt.Sprintf(`<tr><td class="mono"><a href="%s">%s</a></td><td class="mono"><span class="badge badge-muted">%s</span></td><td>%s</td><td><a href="%s" target="_blank" rel="noopener" class="mono" style="font-size:0.8rem;">%s</a></td></tr>`,
		pkgURL, escName, escCh, statusCell, html.EscapeString(inspectURL), inspectLabel)
}

func buildPackageInspectLinkServer(payload unavailableModePayload, item unavailablePackageItem) (string, string) {
	if item.BuildID > 0 {
		serverURL := payload.ServerURL
		if serverURL == "" {
			serverURL = "https://ci.guix.gnu.org"
		}
		if item.Channel == "nonguix" {
			serverURL = payload.NonguixURL
			if serverURL == "" {
				serverURL = "https://cuirass.nonguix.org"
			}
		}
		return fmt.Sprintf("%s/build/%d/details", serverURL, item.BuildID), fmt.Sprintf("Build #%d &rarr;", item.BuildID)
	}
	return buildCuirassSearchURLServer(payload, item), "Cuirass builds &rarr;"
}
