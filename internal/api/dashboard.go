package api

import (
	"fmt"
	"html"
	"strings"
	"time"

	"guix-evaluated/internal/poller"
	"guix-evaluated/internal/resolver"
	"guix-evaluated/internal/store"
)

type systemOverview struct {
	System               string                  `json:"system"`
	StrictReady          *resolver.Result        `json:"strict_ready,omitempty"`
	BestAvailable        *resolver.Result        `json:"best_available,omitempty"`
	NonguixStrictReady   *resolver.Result        `json:"nonguix_strict_ready,omitempty"`
	NonguixBestAvailable *resolver.Result        `json:"nonguix_best_available,omitempty"`
	LatestEval           *evaluationSummaryItem  `json:"latest_eval,omitempty"`
	Evaluations          []evaluationSummaryItem `json:"evaluations"`
}

type dashboardState struct {
	BaseURL        string                    `json:"base_url"`
	ServerURL      string                    `json:"server_url"`
	ActiveSystem   string                    `json:"active_system"`
	RequireNonguix bool                      `json:"require_nonguix,omitempty"`
	Systems        []string                  `json:"systems"`
	Syncing        bool                      `json:"syncing"`
	LastSyncedAt   time.Time                 `json:"last_synced_at"`
	LastPollError  string                    `json:"last_poll_error,omitempty"`
	TotalEvals     int                       `json:"total_evaluations"`
	BySystem       map[string]systemOverview `json:"by_system"`
}

// buildDashboardState assembles the view state across all tracked architectures
// for the HTML dashboard, HTML status page, plain-text summary, and JSON overview.
func buildDashboardState(baseURL string, snap *store.Snapshot, p *poller.Poller) dashboardState {
	st := dashboardState{
		BaseURL:      baseURL,
		ServerURL:    "https://ci.guix.gnu.org",
		ActiveSystem: "x86_64-linux",
		BySystem:     make(map[string]systemOverview),
	}

	if p != nil {
		syncing, _, lastErr := p.Status()
		st.Syncing = syncing
		if lastErr != nil {
			st.LastPollError = lastErr.Error()
		}
	}

	if snap == nil {
		return st
	}

	st.ServerURL = snap.ServerURL
	st.Systems = append([]string(nil), snap.Systems...)
	st.LastSyncedAt = snap.LastSyncedAt
	st.TotalEvals = len(snap.Evaluations)

	for _, sys := range snap.Systems {
		st.BySystem[sys] = buildSystemOverview(snap, sys, 40)
	}
	if _, ok := st.BySystem["x86_64-linux"]; !ok && len(snap.Systems) > 0 {
		st.ActiveSystem = snap.Systems[0]
	}

	return st
}

// buildSystemOverview summarizes up to limit recent evaluations for a target architecture.
// It computes both StrictReady (the newest evaluation with 0 pending builds) and
// BestAvailable (the evaluation with the lowest pending build count, used as a fallback
// when no evaluation in the window has reached 0 pending builds yet).
func buildSystemOverview(snap *store.Snapshot, sys string, limit int) systemOverview {
	ov := systemOverview{
		System:      sys,
		Evaluations: make([]evaluationSummaryItem, 0, limit),
	}
	if snap == nil {
		return ov
	}

	if strict, err := resolver.Resolve(snap, resolver.Query{System: sys, MaxPending: 0, Limit: limit}); err == nil {
		ov.StrictReady = strict
	}
	if ngStrict, err := resolver.Resolve(snap, resolver.Query{
		System:             sys,
		MaxPending:         0,
		Limit:              limit,
		AdditionalChannels: []string{"nonguix"},
	}); err == nil {
		ov.NonguixStrictReady = ngStrict
	} else if ngBest, err := resolver.Resolve(snap, resolver.Query{
		System:             sys,
		MaxPending:         100000,
		Limit:              limit,
		AdditionalChannels: []string{"nonguix"},
	}); err == nil {
		ov.NonguixBestAvailable = ngBest
	}

	var bestIdx = -1
	var minPending = -1
	ngServerURL := snap.ChannelServerURLs["nonguix"]
	if ngServerURL == "" {
		ngServerURL = "https://cuirass.nonguix.org"
	}

	for idx, ev := range snap.Evaluations {
		if len(ov.Evaluations) >= limit {
			break
		}
		data, ok := snap.ByEvalSystem[store.EvalSystemKey{EvalID: ev.ID, System: sys}]
		if !ok || data.TotalJobs == 0 {
			continue
		}

		item := evaluationSummaryItem{
			EvaluationID:  ev.ID,
			Commit:        ev.Commit,
			Timestamp:     ev.Timestamp,
			System:        sys,
			TotalJobs:     data.TotalJobs,
			SucceededJobs: data.SucceededJobs,
			PendingJobs:   data.PendingJobs,
			FailedJobs:    data.FailedJobs,
			PendingSample: data.PendingSample,
			PullReady:     data.PullReady,
			PullEvalID:    data.PullEvalID,
			Complete:      data.Complete,
		}
		for _, ngEv := range snap.ChannelEvaluations["nonguix"] {
			if ngEv.Checkouts["guix"] != ev.Commit {
				continue
			}
			ngData, ok := snap.ByChannelEvalSystem[store.ChannelEvalSystemKey{
				Channel: "nonguix",
				EvalID:  ngEv.ID,
				System:  sys,
			}]
			if !ok || ngData.TotalJobs == 0 {
				continue
			}
			if item.NonguixEvalID == 0 || (!item.NonguixReady && ngData.PendingJobs == 0) {
				item.NonguixEvalID = ngEv.ID
				item.NonguixCommit = ngEv.Commit
				item.NonguixReady = ngData.PendingJobs == 0
				item.NonguixTotalJobs = ngData.TotalJobs
				item.NonguixSucceededJobs = ngData.SucceededJobs
				item.NonguixPendingJobs = ngData.PendingJobs
				item.NonguixFailedJobs = ngData.FailedJobs
				item.NonguixServerURL = ngServerURL
			}
			if item.NonguixReady {
				break
			}
		}
		ov.Evaluations = append(ov.Evaluations, item)
		if ov.LatestEval == nil {
			firstCopy := item
			ov.LatestEval = &firstCopy
		}

		if minPending == -1 || data.PendingJobs < minPending {
			minPending = data.PendingJobs
			bestIdx = idx
		}
	}

	if bestIdx >= 0 {
		ev := snap.Evaluations[bestIdx]
		data := snap.ByEvalSystem[store.EvalSystemKey{EvalID: ev.ID, System: sys}]
		ov.BestAvailable = &resolver.Result{
			Commit:            ev.Commit,
			EvaluationID:      ev.ID,
			EvaluationDate:    ev.Timestamp,
			EvaluationsBehind: bestIdx,
			System:            sys,
			Mode:              resolver.ModeWholeEvaluation,
			PendingCount:      data.PendingJobs,
			FailedCount:       data.FailedJobs,
			SucceededCount:    data.SucceededJobs,
			TotalCount:        data.TotalJobs,
			PullReady:         data.PullReady,
			PullEvaluationID:  data.PullEvalID,
			UntrackedPackages: []string{},
			ServerURL:         snap.ServerURL,
			LastSyncedAt:      snap.LastSyncedAt,
		}
	}

	return ov
}

// renderPlainIndex renders a concise plain-text overview for CLI users (e.g. `curl http://localhost:8080/`).
func renderPlainIndex(st dashboardState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "guix-pull-ready service\n")
	fmt.Fprintf(&b, "========================\n\n")

	hasAnyEval := false
	for _, sys := range st.Systems {
		if ov, ok := st.BySystem[sys]; ok && len(ov.Evaluations) > 0 {
			hasAnyEval = true
			break
		}
	}

	if !hasAnyEval {
		fmt.Fprintf(&b, "Status: Warming up cache from Cuirass (%s)...\n\n", st.ServerURL)
	} else {
		fmt.Fprintf(&b, "Architecture Summary:\n")
		for _, sys := range st.Systems {
			ov := st.BySystem[sys]
			if ov.StrictReady != nil {
				r := ov.StrictReady
				fmt.Fprintf(&b, "  %-16s %s  (eval %d, 0 pending, %d behind master, pull_ready=%v)\n",
					sys+":", r.Commit, r.EvaluationID, r.EvaluationsBehind, r.PullReady)
			} else if ov.BestAvailable != nil {
				r := ov.BestAvailable
				fmt.Fprintf(&b, "  %-16s %s  (eval %d, %d pending [lowest], %d behind master, pull_ready=%v)\n",
					sys+":", r.Commit, r.EvaluationID, r.PendingCount, r.EvaluationsBehind, r.PullReady)
			} else {
				fmt.Fprintf(&b, "  %-16s syncing...\n", sys+":")
			}
		}
		if !st.LastSyncedAt.IsZero() {
			fmt.Fprintf(&b, "\nLast synced: %s (syncing=%v)\n\n",
				st.LastSyncedAt.UTC().Format("2006-01-02 15:04:05 UTC"), st.Syncing)
		}
	}

	fmt.Fprintf(&b, "Usage Examples:\n")
	fmt.Fprintf(&b, "  # 1. Pull latest ready commit for x86_64-linux:\n")
	fmt.Fprintf(&b, "  guix pull -C <(curl -fsSL \"%s/channels.scm\")\n\n", st.BaseURL)
	fmt.Fprintf(&b, "  # 2. Track automatically via ~/.config/guix/channels.scm:\n")
	fmt.Fprintf(&b, "  (downloaded-channels \"%s/channels.scm\")\n\n", st.BaseURL)
	fmt.Fprintf(&b, "  # 3. Inspect detailed JSON status:\n")
	fmt.Fprintf(&b, "  curl -fsSL \"%s/status\"\n", st.BaseURL)
	return b.String()
}

// renderHTMLDashboard renders the main landing page ("/") with the ready commit and CLI usage commands.
func renderHTMLDashboard(st dashboardState) string {
	var b strings.Builder
	ov := st.BySystem[st.ActiveSystem]
	escBaseURL := html.EscapeString(st.BaseURL)
	escServerURL := html.EscapeString(st.ServerURL)

	b.WriteString(renderPageHead("Guix Pull with All Prebuilts Ready"))
	b.WriteString(renderPageHeader("main", st.RequireNonguix))

	b.WriteString(`<main>`)
	b.WriteString(renderReadyCommitBlock(ov, escServerURL, st.RequireNonguix))
	b.WriteString(renderCLIExamplesBlock(escBaseURL, st.RequireNonguix))
	b.WriteString(`</main>`)

	b.WriteString(renderFooter(st))
	b.WriteString(renderCommonScripts(false))
	return b.String()
}

// renderHTMLStatusPage renders the detailed "/status" page containing summary cards,
// the interactive package resolver form, and the recent evaluations table.
func renderHTMLStatusPage(st dashboardState) string {
	var b strings.Builder
	ov := st.BySystem[st.ActiveSystem]
	escServerURL := html.EscapeString(st.ServerURL)

	defaultMaxPending := 0
	if ov.StrictReady == nil && ov.BestAvailable != nil {
		defaultMaxPending = ov.BestAvailable.PendingCount
	}

	b.WriteString(renderPageHead("Guix Pull with All Prebuilts Ready - Status"))
	b.WriteString(renderPageHeader("status", st.RequireNonguix))

	b.WriteString(`<main>`)
	b.WriteString(`<div class="cards">`)
	b.WriteString(renderReadyCommitCardCompact(ov, escServerURL, st.RequireNonguix))
	b.WriteString(renderLatestEvalCard(ov, escServerURL))
	b.WriteString(`</div>`)

	b.WriteString(renderInteractiveResolverPanel(defaultMaxPending, st.RequireNonguix))
	b.WriteString(renderEvaluationsTable(ov, escServerURL, st.RequireNonguix))
	b.WriteString(`</main>`)

	b.WriteString(renderFooter(st))
	b.WriteString(renderCommonScripts(true))
	return b.String()
}

func renderPageHead(title string) string {
	return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + html.EscapeString(title) + `</title>
<style>
	:root {
		--bg: #ffffff;
		--border: #e2e8f0;
		--text: #0f172a;
		--muted: #475569;
		--accent: #2563eb;
		--accent-hover: #1d4ed8;
		--ok-bg: #dcfce7;
		--ok-text: #15803d;
		--ok-bar: #22c55e;
		--warn-bg: #fef9c3;
		--warn-text: #a16207;
		--warn-bar: #eab308;
		--err-bg: #fee2e2;
		--err-text: #b91c1c;
		--err-bar: #ef4444;
		--code-bg: #f1f5f9;
	}
	* { box-sizing: border-box; }
	body {
		margin: 0 auto;
		max-width: 1120px;
		padding: 1.5rem 1rem 3rem;
		font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
		background: var(--bg);
		color: var(--text);
		line-height: 1.5;
	}
	code, pre, .mono {
		font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
	}
	a { color: var(--accent); text-decoration: none; }
	a:hover { text-decoration: underline; }

	/* Site Header */
	header.site-header-container {
		display: flex;
		align-items: center;
		justify-content: space-between;
		flex-wrap: wrap;
		padding-bottom: 0.85rem;
		border-bottom: 1px solid var(--border);
		margin-bottom: 1.5rem;
		width: 100%;
	}
	.site-header-element-left {
		font-size: 1.25rem;
		font-weight: 700;
	}
	.site-header-element-left a {
		color: var(--text);
		text-decoration: none;
	}
	.site-header-element-right {
		text-align: right;
		font-size: 0.95rem;
	}
	.site-header-element-right a {
		color: var(--muted);
		text-decoration: none;
		font-weight: 500;
		padding: 0.15rem 0.4rem;
	}
	.site-header-element-right a:hover {
		color: var(--accent);
		text-decoration: underline;
	}
	.site-header-element-right a.active {
		color: var(--text);
		font-weight: 700;
		text-decoration: none;
	}
	@media (max-width: 500px) {
		.site-header-element-left,
		.site-header-element-right {
			flex-basis: 100%;
			text-align: left;
		}
		.site-header-element-right {
			margin-top: 0.4rem;
		}
	}

	/* Main Container */
	main {
		width: 100%;
		display: flex;
		flex-direction: column;
		gap: 1.5rem;
		min-width: 0;
	}

	/* Badges */
	.badge {
		display: inline-flex;
		align-items: center;
		gap: 0.35rem;
		padding: 0.2rem 0.55rem;
		border-radius: 999px;
		font-size: 0.78rem;
		font-weight: 600;
		white-space: nowrap;
	}
	.badge-ok { background: var(--ok-bg); color: var(--ok-text); }
	.badge-warn { background: var(--warn-bg); color: var(--warn-text); }
	.badge-err { background: var(--err-bg); color: var(--err-text); }
	.badge-neutral { background: var(--code-bg); color: var(--muted); border: 1px solid var(--border); }

	/* Cards & Panels */
	.cards {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
		gap: 1rem;
		width: 100%;
	}
	.card, .panel {
		background: var(--bg);
		border: 1px solid var(--border);
		border-radius: 8px;
		padding: 1.25rem;
		box-shadow: 0 1px 2px rgba(0,0,0,0.03);
		width: 100%;
		min-width: 0;
	}
	.card-title {
		font-size: 0.82rem;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--muted);
		font-weight: 600;
		margin: 0 0 0.5rem;
		display: flex;
		justify-content: space-between;
		align-items: center;
	}
	.card-commit {
		font-size: 1.05rem;
		font-weight: 700;
		margin: 0.25rem 0 0.5rem;
		word-break: break-all;
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.5rem;
		min-width: 0;
	}
	.card-meta { font-size: 0.85rem; color: var(--muted); margin-bottom: 0.5rem; }
	.btn-row { display: flex; flex-wrap: wrap; gap: 0.4rem; margin-top: 0.5rem; }
	button, .btn {
		padding: 0.32rem 0.65rem;
		font-size: 0.8rem;
		border-radius: 5px;
		border: 1px solid var(--border);
		background: var(--code-bg);
		color: var(--text);
		cursor: pointer;
		font-weight: 500;
		white-space: nowrap;
	}
	button:hover, .btn:hover { background: #e2e8f0; }
	button.primary {
		background: var(--accent);
		color: #ffffff;
		border-color: var(--accent);
	}
	button.primary:hover { background: var(--accent-hover); }

	/* Examples List (Code Block + Copy Button) */
	.examples-list {
		display: flex;
		flex-direction: column;
		gap: 0.85rem;
		margin-top: 0.75rem;
		width: 100%;
		min-width: 0;
	}
	.example-item {
		display: flex;
		flex-direction: column;
		gap: 0.3rem;
		width: 100%;
		min-width: 0;
	}
	.example-desc {
		font-size: 0.85rem;
		font-weight: 600;
		color: var(--muted);
	}
	.code-box-row {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		background: var(--code-bg);
		border: 1px solid var(--border);
		border-radius: 6px;
		padding: 0.4rem 0.6rem;
		width: 100%;
		min-width: 0;
	}
	.code-box-row pre {
		margin: 0;
		flex: 1;
		min-width: 0;
		overflow-x: auto;
		font-size: 0.84rem;
		background: transparent;
		padding: 0.2rem 0;
		border: none;
		color: var(--text);
	}
	.code-box-row button {
		flex-shrink: 0;
		padding: 0.3rem 0.7rem;
		font-size: 0.78rem;
	}

	/* Interactive Resolver Panel */
	.panel h2 { margin: 0 0 0.35rem; font-size: 1.1rem; }
	.panel p.sub { margin: 0 0 1rem; color: var(--muted); font-size: 0.88rem; }
	.form-grid {
		display: grid;
		grid-template-columns: 1fr auto auto;
		gap: 0.75rem;
		align-items: end;
	}
	@media (max-width: 760px) {
		.form-grid { grid-template-columns: 1fr; }
	}
	.field label { display: block; font-size: 0.8rem; font-weight: 600; color: var(--muted); margin-bottom: 0.25rem; }
	.field input[type="text"], .field input[type="number"] {
		width: 100%;
		padding: 0.48rem 0.65rem;
		border: 1px solid var(--border);
		border-radius: 6px;
		font-size: 0.9rem;
		font-family: inherit;
	}
	.checkboxes { display: flex; flex-wrap: wrap; gap: 1rem; margin-top: 0.65rem; font-size: 0.86rem; color: var(--muted); }
	.checkboxes label { display: inline-flex; align-items: center; gap: 0.35rem; cursor: pointer; }
	#finder-result { margin-top: 1rem; display: none; }
	pre.code-box {
		background: var(--code-bg);
		border: 1px solid var(--border);
		border-radius: 6px;
		padding: 0.75rem 0.9rem;
		overflow-x: auto;
		font-size: 0.83rem;
		margin: 0.5rem 0 0;
	}

	/* Evaluations Table */
	.table-wrap { overflow-x: auto; width: 100%; }
	table { border-collapse: collapse; width: 100%; font-size: 0.88rem; }
	th, td { border-bottom: 1px solid var(--border); padding: 0.65rem 0.75rem; text-align: left; vertical-align: middle; }
	th { background: var(--code-bg); color: var(--muted); font-weight: 600; font-size: 0.78rem; text-transform: uppercase; letter-spacing: 0.03em; }
	tr:hover td { background: #f8fafc; }

	/* Progress Bar */
	.prog-cell { min-width: 210px; }
	.prog-bar {
		display: flex;
		height: 8px;
		width: 100%;
		background: var(--code-bg);
		border-radius: 999px;
		overflow: hidden;
		margin-bottom: 0.3rem;
	}
	.prog-ok { background: var(--ok-bar); }
	.prog-warn { background: var(--warn-bar); }
	.prog-err { background: var(--err-bar); }
	.prog-legend { font-size: 0.76rem; color: var(--muted); display: flex; gap: 0.5rem; }

	.pending-tags { display: flex; flex-wrap: wrap; gap: 0.25rem; max-width: 290px; }
	.pkg-tag {
		background: var(--code-bg);
		border: 1px solid var(--border);
		border-radius: 4px;
		padding: 0.08rem 0.38rem;
		font-size: 0.74rem;
		color: var(--muted);
	}
	.local-time[data-utc]:not([data-formatted]) { visibility: hidden; }

	/* Footer */
	footer {
		display: flex;
		justify-content: space-between;
		align-items: center;
		flex-wrap: wrap;
		gap: 0.5rem;
		margin-top: 2.5rem;
		padding-top: 1rem;
		border-top: 1px solid var(--border);
		font-size: 0.82rem;
		color: var(--muted);
		width: 100%;
	}
	.site-footer-element-left {
		flex: 1;
	}
	.site-footer-element-right {
		text-align: right;
	}
	@media (max-width: 500px) {
		.site-footer-element-left,
		.site-footer-element-right {
			flex-basis: 100%;
			text-align: left;
		}
	}
</style>
<noscript><style>.local-time[data-utc] { visibility: visible !important; }</style></noscript>
<script>
function formatLocalTimes() {
	const pad = (n) => String(n).padStart(2, "0");
	let tzAbbr = "";
	try {
		const parts = new Intl.DateTimeFormat(undefined, { timeZoneName: "short" }).formatToParts(new Date());
		const tzPart = parts.find((p) => p.type === "timeZoneName");
		if (tzPart && tzPart.value) tzAbbr = " " + tzPart.value;
	} catch (_) {}

	document.querySelectorAll(".local-time[data-utc]:not([data-formatted])").forEach((el) => {
		const raw = el.getAttribute("data-utc");
		if (!raw) return;
		const d = new Date(raw);
		if (isNaN(d.getTime())) {
			el.setAttribute("data-formatted", "1");
			return;
		}
		const y = d.getFullYear();
		const m = pad(d.getMonth() + 1);
		const day = pad(d.getDate());
		const hh = pad(d.getHours());
		const mm = pad(d.getMinutes());
		if (el.getAttribute("data-show-sec") === "1") {
			const ss = pad(d.getSeconds());
			el.textContent = y + "-" + m + "-" + day + " " + hh + ":" + mm + ":" + ss + tzAbbr;
		} else {
			el.textContent = y + "-" + m + "-" + day + " " + hh + ":" + mm;
		}
		el.setAttribute("data-formatted", "1");
	});
}
</script>
</head>
<body>
`
}

func renderPageHeader(currentTab string, requireNonguix bool) string {
	statusClass := ""
	if currentTab == "status" {
		statusClass = " class=\"active\""
	}
	querySuffix := ""
	if requireNonguix {
		querySuffix = "?additional_channels=nonguix"
	}

	return fmt.Sprintf(`
	<header class="site-header-container">
		<strong class="site-header-element-left"><a href="/%s">Guix Pull with All Prebuilts Ready</a></strong>
		<nav class="site-header-element-right">
			<a href="/status%s"%s>Status</a>
		</nav>
	</header>
`, querySuffix, querySuffix, statusClass)
}

func renderReadyCommitBlock(ov systemOverview, escServerURL string, requireNonguix bool) string {
	var b strings.Builder
	checkedAttr := ""
	guixStyle := ""
	ngStyle := ` style="display:none;"`
	if requireNonguix {
		checkedAttr = " checked"
		guixStyle = ` style="display:none;"`
		ngStyle = ""
	}
	b.WriteString(`<div class="panel">`)
	b.WriteString(`<div style="display:flex;justify-content:space-between;align-items:center;flex-wrap:wrap;gap:0.5rem;margin-bottom:0.5rem;">`)
	b.WriteString(`<h2 style="margin:0;">Latest Commit with All Prebuilts Ready on Substitutes</h2>`)
	fmt.Fprintf(&b, `<label style="font-size:0.88rem;font-weight:500;color:var(--muted);cursor:pointer;display:inline-flex;align-items:center;gap:0.35rem;"><input type="checkbox" id="main-nonguix-toggle"%s onchange="toggleMainNonguix(this.checked)"> Require <code>nonguix</code> channel</label>`, checkedAttr)
	b.WriteString(`</div>`)

	fmt.Fprintf(&b, `<div id="main-commit-guix"%s>`, guixStyle)
	if ov.StrictReady != nil {
		r := ov.StrictReady
		dateHTML := formatEvalDateHTML(r.EvaluationDate)
		fmt.Fprintf(&b, `
		<div class="card-commit mono">
			<a href="https://codeberg.org/guix/guix/commit/%s" target="_blank" rel="noopener">%s</a>
			<button type="button" class="btn" data-copy="%s" onclick="copyFromBtn(this)">Copy Commit</button>
		</div>
		<div class="card-meta">Eval <a href="%s/eval/%d" target="_blank" rel="noopener">#%d</a>%s &middot; %d behind master &middot; %d packages built</div>`,
			html.EscapeString(r.Commit), html.EscapeString(r.Commit),
			html.EscapeString(r.Commit),
			escServerURL, r.EvaluationID, r.EvaluationID, dateHTML, r.EvaluationsBehind, r.SucceededCount,
		)
	} else if ov.BestAvailable != nil {
		r := ov.BestAvailable
		dateHTML := formatEvalDateHTML(r.EvaluationDate)
		fmt.Fprintf(&b, `
		<div style="margin-bottom:0.35rem;">
			<span class="badge badge-warn">Closest to Ready (%d Pending, no 0-pending in last %d)</span>
		</div>
		<div class="card-commit mono">
			<a href="https://codeberg.org/guix/guix/commit/%s" target="_blank" rel="noopener">%s</a>
			<button type="button" class="btn" data-copy="%s" onclick="copyFromBtn(this)">Copy Commit</button>
		</div>
		<div class="card-meta">Eval <a href="%s/eval/%d" target="_blank" rel="noopener">#%d</a>%s &middot; %d behind master &middot; %d packages built</div>`,
			r.PendingCount, len(ov.Evaluations),
			html.EscapeString(r.Commit), html.EscapeString(r.Commit),
			html.EscapeString(r.Commit),
			escServerURL, r.EvaluationID, r.EvaluationID, dateHTML, r.EvaluationsBehind, r.SucceededCount,
		)
	} else {
		b.WriteString(`<div class="card-meta">Initial evaluation sync from Cuirass is in progress.</div>`)
	}
	b.WriteString(`</div>`)

	fmt.Fprintf(&b, `<div id="main-commit-nonguix"%s>`, ngStyle)
	ngRes := ov.NonguixStrictReady
	isNgFallback := false
	if ngRes == nil && ov.NonguixBestAvailable != nil {
		ngRes = ov.NonguixBestAvailable
		isNgFallback = true
	}
	if ngRes != nil {
		if isNgFallback {
			fmt.Fprintf(&b, `<div style="margin-bottom:0.35rem;"><span class="badge badge-warn">Closest Coordinated Evaluation (%d Pending on Guix)</span></div>`, ngRes.PendingCount)
		}
		guixDateHTML := formatEvalDateHTML(ngRes.EvaluationDate)
		fmt.Fprintf(&b, `
		<div style="font-size:0.82rem;font-weight:600;color:var(--muted);margin-top:0.25rem;">guix channel:</div>
		<div class="card-commit mono">
			<a href="https://codeberg.org/guix/guix/commit/%s" target="_blank" rel="noopener">%s</a>
			<button type="button" class="btn" data-copy="%s" onclick="copyFromBtn(this)">Copy Commit</button>
		</div>
		<div class="card-meta">Eval <a href="%s/eval/%d" target="_blank" rel="noopener">#%d</a>%s &middot; %d behind master &middot; %d packages built</div>`,
			html.EscapeString(ngRes.Commit), html.EscapeString(ngRes.Commit),
			html.EscapeString(ngRes.Commit),
			escServerURL, ngRes.EvaluationID, ngRes.EvaluationID, guixDateHTML, ngRes.EvaluationsBehind, ngRes.SucceededCount,
		)
		for _, ch := range ngRes.AdditionalChannels {
			chDateHTML := formatEvalDateHTML(ch.EvaluationDate)
			chServer := html.EscapeString(ch.ServerURL)
			fmt.Fprintf(&b, `
		<div style="font-size:0.82rem;font-weight:600;color:var(--muted);margin-top:0.65rem;">%s channel:</div>
		<div class="card-commit mono">
			<a href="https://gitlab.com/nonguix/nonguix/-/commit/%s" target="_blank" rel="noopener">%s</a>
			<button type="button" class="btn" data-copy="%s" onclick="copyFromBtn(this)">Copy Commit</button>
		</div>
		<div class="card-meta">Eval <a href="%s/eval/%d" target="_blank" rel="noopener">#%d</a>%s &middot; %d behind %s &middot; %d packages built</div>`,
				html.EscapeString(ch.Channel),
				html.EscapeString(ch.Commit), html.EscapeString(ch.Commit),
				html.EscapeString(ch.Commit),
				chServer, ch.EvaluationID, ch.EvaluationID, chDateHTML, ch.EvaluationsBehind, html.EscapeString(ch.Channel), ch.SucceededCount,
			)
		}
	} else {
		b.WriteString(`<div class="card-meta">Waiting for coordinated nonguix + guix evaluation data...</div>`)
	}
	b.WriteString(`</div>`)

	b.WriteString(`</div>`)
	return b.String()
}

func formatEvalDateHTML(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	utcStr := t.UTC().Format("2006-01-02 15:04:05 UTC")
	shortUTC := t.UTC().Format("2006-01-02 15:04")
	isoStr := t.UTC().Format(time.RFC3339)
	return fmt.Sprintf(` &middot; <span class="local-time" data-utc="%s" title="%s">%s</span>`,
		html.EscapeString(isoStr), html.EscapeString(utcStr), html.EscapeString(shortUTC))
}

func renderCLIExamplesBlock(escBaseURL string, requireNonguix bool) string {
	var b strings.Builder
	guixStyle := ""
	ngStyle := ` style="display:none;"`
	if requireNonguix {
		guixStyle = ` style="display:none;"`
		ngStyle = ""
	}
	b.WriteString(`<div class="panel">`)
	b.WriteString(`<h2>Usages</h2>`)

	fmt.Fprintf(&b, `
		<div id="main-cli-guix" class="examples-list"%s>
			<div class="example-item">
				<div class="example-desc">Pull the newest ready commit dynamically via API:</div>
				<div class="code-box-row">
					<pre class="mono">guix pull -C &lt;(curl -fsSL "%s/channels.scm")</pre>
					<button type="button" class="btn" data-copy="guix pull -C &lt;(curl -fsSL &quot;%s/channels.scm&quot;)" onclick="copyFromBtn(this)">Copy</button>
				</div>
			</div>
			<div class="example-item">
				<div class="example-desc">Track automatically via ~/.config/guix/channels.scm:</div>
				<div class="code-box-row">
					<pre class="mono">(downloaded-channels "%s/channels.scm")</pre>
					<button type="button" class="btn" data-copy="(downloaded-channels &quot;%s/channels.scm&quot;)" onclick="copyFromBtn(this)">Copy</button>
				</div>
				<div style="margin-top:0.35rem;font-size:0.8rem;color:var(--text-muted);">See GNU Guix manual on <a href="https://guix.gnu.org/manual/devel/en/html_node/Channels-with-Substitutes.html" target="_blank" rel="noopener">Channels with Substitutes</a>.</div>
			</div>
		</div>`,
		guixStyle,
		escBaseURL, escBaseURL,
		escBaseURL, escBaseURL,
	)

	fmt.Fprintf(&b, `
		<div id="main-cli-nonguix" class="examples-list"%s>
			<div style="padding:0.65rem 0.85rem;background:var(--warn-bg);color:var(--warn-text);border-radius:6px;font-size:0.86rem;">
				<strong>IMPORTANT:</strong> <code>nonguix</code> substitutes require one-time manual setup. Follow <a href="https://gitlab.com/nonguix/nonguix/#substitutes-for-nonguix" target="_blank" rel="noopener" style="color:inherit;font-weight:600;text-decoration:underline;">Substitutes for Nonguix</a> to authorize the substitute server.
			</div>
			<div class="example-item">
				<div class="example-desc">Pull both guix and nonguix at their coordinated ready commits:</div>
				<div class="code-box-row">
					<pre class="mono">guix pull -C &lt;(curl -fsSL "%s/channels.scm?additional_channels=nonguix")</pre>
					<button type="button" class="btn" data-copy="guix pull -C &lt;(curl -fsSL &quot;%s/channels.scm?additional_channels=nonguix&quot;)" onclick="copyFromBtn(this)">Copy</button>
				</div>
			</div>
			<div class="example-item">
				<div class="example-desc">Track both channels automatically via ~/.config/guix/channels.scm:</div>
				<div class="code-box-row">
					<pre class="mono">(downloaded-channels "%s/channels.scm?additional_channels=nonguix")</pre>
					<button type="button" class="btn" data-copy="(downloaded-channels &quot;%s/channels.scm?additional_channels=nonguix&quot;)" onclick="copyFromBtn(this)">Copy</button>
				</div>
				<div style="margin-top:0.35rem;font-size:0.8rem;color:var(--text-muted);">See GNU Guix manual on <a href="https://guix.gnu.org/manual/devel/en/html_node/Channels-with-Substitutes.html" target="_blank" rel="noopener">Channels with Substitutes</a>.</div>
			</div>
		</div>
	</div>`,
		ngStyle,
		escBaseURL, escBaseURL,
		escBaseURL, escBaseURL,
	)
	return b.String()
}

func renderReadyCommitCardCompact(ov systemOverview, escServerURL string, requireNonguix bool) string {
	var b strings.Builder
	guixStyle := ""
	ngStyle := ` style="display:none;"`
	if requireNonguix {
		guixStyle = ` style="display:none;"`
		ngStyle = ""
	}
	b.WriteString(`<div class="card">`)
	fmt.Fprintf(&b, `<div id="status-commit-guix"%s>`, guixStyle)
	if ov.StrictReady != nil {
		r := ov.StrictReady
		short := shortCommit(r.Commit)
		dateHTML := formatEvalDateHTML(r.EvaluationDate)
		fmt.Fprintf(&b, `
			<div class="card-title"><span>Latest Ready Commit</span></div>
			<div class="card-commit mono">
				<a href="https://codeberg.org/guix/guix/commit/%s" target="_blank" rel="noopener">%s</a>
				<button type="button" class="btn" data-copy="%s" onclick="copyFromBtn(this)">Copy</button>
			</div>
			<div class="card-meta">Eval <a href="%s/eval/%d" target="_blank" rel="noopener">#%d</a>%s &middot; %d behind &middot; %d built</div>`,
			html.EscapeString(r.Commit), html.EscapeString(short),
			html.EscapeString(r.Commit),
			escServerURL, r.EvaluationID, r.EvaluationID, dateHTML, r.EvaluationsBehind, r.SucceededCount,
		)
	} else if ov.BestAvailable != nil {
		r := ov.BestAvailable
		short := shortCommit(r.Commit)
		dateHTML := formatEvalDateHTML(r.EvaluationDate)
		fmt.Fprintf(&b, `
			<div class="card-title"><span>Closest to Ready (%d Pending)</span><span class="badge badge-warn">No 0-pending in last %d</span></div>
			<div class="card-commit mono">
				<a href="https://codeberg.org/guix/guix/commit/%s" target="_blank" rel="noopener">%s</a>
				<button type="button" class="btn" data-copy="%s" onclick="copyFromBtn(this)">Copy</button>
			</div>
			<div class="card-meta">Eval <a href="%s/eval/%d" target="_blank" rel="noopener">#%d</a>%s &middot; %d behind &middot; %d built</div>`,
			r.PendingCount, len(ov.Evaluations),
			html.EscapeString(r.Commit), html.EscapeString(short),
			html.EscapeString(r.Commit),
			escServerURL, r.EvaluationID, r.EvaluationID, dateHTML, r.EvaluationsBehind, r.SucceededCount,
		)
	} else {
		b.WriteString(`
			<div class="card-title"><span>Latest Ready Commit</span><span class="badge badge-warn">Warming up</span></div>
			<div class="card-meta">Initial evaluation sync from Cuirass is in progress.</div>`)
	}
	b.WriteString(`</div>`)

	fmt.Fprintf(&b, `<div id="status-commit-nonguix"%s>`, ngStyle)
	ngRes := ov.NonguixStrictReady
	isNgFallback := false
	if ngRes == nil && ov.NonguixBestAvailable != nil {
		ngRes = ov.NonguixBestAvailable
		isNgFallback = true
	}
	if ngRes != nil {
		shortGuix := shortCommit(ngRes.Commit)
		guixDateHTML := formatEvalDateHTML(ngRes.EvaluationDate)
		titleBadge := `<span class="badge badge-ok">guix + nonguix</span>`
		if isNgFallback {
			titleBadge = fmt.Sprintf(`<span class="badge badge-warn">%d Pending</span>`, ngRes.PendingCount)
		}
		fmt.Fprintf(&b, `
			<div class="card-title"><span>Latest Ready Commit (Coordinated)</span>%s</div>
			<div class="card-commit mono">
				<span style="font-size:0.78rem;color:var(--muted);">guix:</span> <a href="https://codeberg.org/guix/guix/commit/%s" target="_blank" rel="noopener">%s</a>
				<button type="button" class="btn" data-copy="%s" onclick="copyFromBtn(this)">Copy</button>
			</div>
			<div class="card-meta">Eval <a href="%s/eval/%d" target="_blank" rel="noopener">#%d</a>%s &middot; %d behind &middot; %d built</div>`,
			titleBadge,
			html.EscapeString(ngRes.Commit), html.EscapeString(shortGuix),
			html.EscapeString(ngRes.Commit),
			escServerURL, ngRes.EvaluationID, ngRes.EvaluationID, guixDateHTML, ngRes.EvaluationsBehind, ngRes.SucceededCount,
		)
		for _, ch := range ngRes.AdditionalChannels {
			shortCh := shortCommit(ch.Commit)
			chDateHTML := formatEvalDateHTML(ch.EvaluationDate)
			chServer := html.EscapeString(ch.ServerURL)
			fmt.Fprintf(&b, `
			<div class="card-commit mono" style="margin-top:0.35rem;">
				<span style="font-size:0.78rem;color:var(--muted);">%s:</span> <a href="https://gitlab.com/nonguix/nonguix/-/commit/%s" target="_blank" rel="noopener">%s</a>
				<button type="button" class="btn" data-copy="%s" onclick="copyFromBtn(this)">Copy</button>
			</div>
			<div class="card-meta">Eval <a href="%s/eval/%d" target="_blank" rel="noopener">#%d</a>%s &middot; %d behind &middot; %d built</div>`,
				html.EscapeString(ch.Channel),
				html.EscapeString(ch.Commit), html.EscapeString(shortCh),
				html.EscapeString(ch.Commit),
				chServer, ch.EvaluationID, ch.EvaluationID, chDateHTML, ch.EvaluationsBehind, ch.SucceededCount,
			)
		}
	} else {
		b.WriteString(`
			<div class="card-title"><span>Latest Ready Commit (Coordinated)</span><span class="badge badge-warn">Warming up</span></div>
			<div class="card-meta">Waiting for coordinated nonguix + guix evaluation data...</div>`)
	}
	b.WriteString(`</div>`)
	b.WriteString(`</div>`)
	return b.String()
}

func renderLatestEvalCard(ov systemOverview, escServerURL string) string {
	var b strings.Builder
	b.WriteString(`<div class="card">`)
	if ov.LatestEval != nil {
		le := ov.LatestEval
		short := shortCommit(le.Commit)
		pct := 0.0
		if le.TotalJobs > 0 {
			pct = float64(le.SucceededJobs) * 100.0 / float64(le.TotalJobs)
		}
		fmt.Fprintf(&b, `
			<div class="card-title"><span>Latest Master Evaluation</span><span class="badge badge-neutral">%.1f%% Built</span></div>
			<div class="card-commit mono">
				<a href="https://codeberg.org/guix/guix/commit/%s" target="_blank" rel="noopener">%s</a>
				<button type="button" class="btn" data-copy="%s" onclick="copyFromBtn(this)">Copy</button>
			</div>
			<div class="card-meta">Eval <a href="%s/eval/%d" target="_blank" rel="noopener">#%d</a> &middot; %d ok / %d pend / %d fail</div>`,
			pct,
			html.EscapeString(le.Commit), html.EscapeString(short),
			html.EscapeString(le.Commit),
			escServerURL, le.EvaluationID, le.EvaluationID, le.SucceededJobs, le.PendingJobs, le.FailedJobs,
		)
	} else {
		b.WriteString(`
			<div class="card-title"><span>Latest Master Evaluation</span></div>
			<div class="card-meta">Waiting for evaluation data...</div>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

func renderInteractiveResolverPanel(defaultMaxPending int, requireNonguix bool) string {
	checkedAttr := ""
	if requireNonguix {
		checkedAttr = " checked"
	}
	return fmt.Sprintf(`
	<div class="panel">
		<h2>Interactive Commit &amp; Package Resolver</h2>
		<p class="sub">Find the newest commit where your specific packages (or the whole evaluation) have substitutes ready.</p>
		<form id="finder-form" onsubmit="runFinder(event)">
			<div class="form-grid">
				<div class="field">
					<label for="f-pkgs">Packages (comma or space separated, leave empty for whole-evaluation mode)</label>
					<input type="text" id="f-pkgs" name="pkgs" class="mono" placeholder="e.g. emacs, git, linux">
				</div>
				<div class="field" style="width:150px;">
					<label for="f-max-pending">Max Pending (whole-eval)</label>
					<input type="number" id="f-max-pending" name="max_pending" min="0" max="100000" value="%d" class="mono">
				</div>
				<div>
					<button type="submit" class="primary" style="padding:0.52rem 1rem;font-size:0.9rem;">Find Ready Commit</button>
				</div>
			</div>
			<div class="checkboxes">
				<label><input type="checkbox" id="f-additional-nonguix"%s onchange="toggleStatusNonguix(this.checked)"> Require <code>nonguix</code> channel</label>
				<label><input type="checkbox" id="f-allow-failed"> Allow failed builds in package mode</label>
			</div>
		</form>
		<div id="finder-result"></div>
	</div>
`, defaultMaxPending, checkedAttr)
}

func renderEvaluationsTable(ov systemOverview, escServerURL string, requireNonguix bool) string {
	var b strings.Builder
	ngColStyle := ` style="display:none;"`
	if requireNonguix {
		ngColStyle = ""
	}
	fmt.Fprintf(&b, `
	<div class="panel">
		<h2>Recent Master Evaluations</h2>
		<p class="sub">Showing %d cached evaluations ordered from newest to oldest.</p>
`, len(ov.Evaluations))

	if len(ov.Evaluations) == 0 {
		b.WriteString(`<p>No evaluations cached yet. Background sync is running...</p>`)
	} else {
		fmt.Fprintf(&b, `
		<div class="table-wrap">
			<table>
				<thead>
					<tr>
						<th>guix commit</th>
						<th>Date (Local)</th>
						<th><a href="https://ci.guix.gnu.org/jobset/master" target="_blank" rel="noopener">guix/master</a></th>
						<th><a href="https://ci.guix.gnu.org/jobset/guix" target="_blank" rel="noopener">guix/guix</a></th>
						<th class="col-nonguix"%s><a href="https://cuirass.nonguix.org/jobset/nonguix" target="_blank" rel="noopener">nonguix/nonguix</a></th>
					</tr>
				</thead>
				<tbody>
`, ngColStyle)
		for _, item := range ov.Evaluations {
			short := shortCommit(item.Commit)
			dateCell := `<td class="mono" style="white-space:nowrap;">unknown</td>`
			if !item.Timestamp.IsZero() {
				utcStr := item.Timestamp.UTC().Format("2006-01-02 15:04:05 UTC")
				shortUTC := item.Timestamp.UTC().Format("2006-01-02 15:04")
				isoStr := item.Timestamp.UTC().Format(time.RFC3339)
				dateCell = fmt.Sprintf(`<td class="mono local-time" style="white-space:nowrap;" data-utc="%s" title="%s">%s</td>`,
					html.EscapeString(isoStr), html.EscapeString(utcStr), html.EscapeString(shortUTC))
			}

			pullBadge := `<span class="badge badge-warn">Pending</span>`
			if item.PullReady {
				pullBadge = `<span class="badge badge-ok">Ready</span>`
			}
			pullCell := pullBadge
			if item.PullEvalID > 0 {
				pullCell = fmt.Sprintf(`<div style="display:inline-flex;align-items:center;gap:0.4rem;"><a href="%s/eval/%d" target="_blank" rel="noopener" class="mono"><strong>#%d</strong></a>%s</div>`,
					escServerURL, item.PullEvalID, item.PullEvalID, pullBadge)
			}

			masterBadge := `<span class="badge badge-warn">Pending</span>`
			if item.PendingJobs == 0 {
				masterBadge = `<span class="badge badge-ok">Ready</span>`
			}
			masterCell := fmt.Sprintf(`<div style="display:inline-flex;align-items:center;gap:0.4rem;"><a href="%s/eval/%d" target="_blank" rel="noopener" class="mono"><strong>#%d</strong></a>%s</div>`,
				escServerURL, item.EvaluationID, item.EvaluationID, masterBadge)

			nonguixCell := `<span style="color:var(--muted);font-size:0.8rem;" title="No nonguix evaluation against this Guix commit">-</span>`
			if item.NonguixEvalID > 0 {
				escNgServer := html.EscapeString(item.NonguixServerURL)
				shortNgCommit := html.EscapeString(shortCommit(item.NonguixCommit))
				ngBadge := `<span class="badge badge-warn">Pending</span>`
				if item.NonguixReady {
					ngBadge = `<span class="badge badge-ok">Ready</span>`
				}
				nonguixCell = fmt.Sprintf(`<div style="display:inline-flex;align-items:center;gap:0.4rem;"><a href="%s/eval/%d" target="_blank" rel="noopener" class="mono" title="nonguix commit %s"><strong>#%d</strong></a>%s</div>`,
					escNgServer, item.NonguixEvalID, shortNgCommit, item.NonguixEvalID, ngBadge)
			}

			fmt.Fprintf(&b, `
					<tr>
						<td class="mono">
							<a href="https://codeberg.org/guix/guix/commit/%s" target="_blank" rel="noopener">%s</a>
							<button type="button" data-copy="%s" onclick="copyFromBtn(this)" style="padding:0.1rem 0.4rem;font-size:0.72rem;margin-left:0.3rem;">Copy</button>
						</td>
						%s
						<td>%s</td>
						<td>%s</td>
						<td class="col-nonguix"%s>%s</td>
					</tr>`,
				html.EscapeString(item.Commit), html.EscapeString(short), html.EscapeString(item.Commit),
				dateCell,
				masterCell,
				pullCell,
				ngColStyle,
				nonguixCell,
			)
		}
		b.WriteString(`
				</tbody>
			</table>
		</div>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

func renderFooter(st dashboardState) string {
	escServerURL := html.EscapeString(st.ServerURL)
	lastSyncHTML := "never"
	if !st.LastSyncedAt.IsZero() {
		utcStr := st.LastSyncedAt.UTC().Format("2006-01-02 15:04:05 UTC")
		isoStr := st.LastSyncedAt.UTC().Format(time.RFC3339)
		lastSyncHTML = fmt.Sprintf(`<span class="local-time" data-utc="%s" data-show-sec="1" title="%s">%s</span>`,
			html.EscapeString(isoStr), html.EscapeString(utcStr), html.EscapeString(utcStr))
	}

	return fmt.Sprintf(`
	<footer>
		<div class="site-footer-element-left">
			Substitutes tracked from <a href="%s" target="_blank" rel="noopener"><code>%s</code></a> &middot; Last sync: %s
		</div>
		<div class="site-footer-element-right">
			<a href="https://github.com/whhone/guix-pull-ready" target="_blank" rel="noopener">Source Code</a>
		</div>
	</footer>
`, escServerURL, escServerURL, lastSyncHTML)
}

func renderCommonScripts(includeFinder bool) string {
	var b strings.Builder

	b.WriteString(`
<script>
formatLocalTimes();

function copyFromBtn(btn) {
	const text = btn.getAttribute("data-copy");
	if (text) {
		copyText(text, btn);
	}
}

function copyText(text, btn) {
	const orig = btn.textContent;
	const markCopied = () => {
		btn.textContent = "Copied!";
		setTimeout(() => { btn.textContent = orig; }, 1400);
	};

	if (navigator.clipboard && navigator.clipboard.writeText) {
		navigator.clipboard.writeText(text).then(markCopied).catch(() => fallbackCopy(text, markCopied));
	} else {
		fallbackCopy(text, markCopied);
	}
}

function fallbackCopy(text, onDone) {
	const ta = document.createElement("textarea");
	ta.value = text;
	ta.style.position = "fixed";
	ta.style.opacity = "0";
	document.body.appendChild(ta);
	ta.focus();
	ta.select();
	try {
		if (document.execCommand("copy")) {
			onDone();
		}
	} finally {
		document.body.removeChild(ta);
	}
}

function escapeHtml(s) {
	return String(s)
		.replace(/&/g, "&amp;")
		.replace(/</g, "&lt;")
		.replace(/>/g, "&gt;")
		.replace(/"/g, "&quot;")
		.replace(/'/g, "&#39;");
}

function syncNonguixURLParam(checked) {
	if (!window.history || !window.history.replaceState) return;
	const url = new URL(window.location.href);
	if (checked) {
		url.searchParams.set("additional_channels", "nonguix");
	} else {
		url.searchParams.delete("additional_channels");
	}
	window.history.replaceState(null, "", url.pathname + url.search + url.hash);
	const homeLink = document.querySelector(".site-header-element-left a");
	const statusLink = document.querySelector(".site-header-element-right a");
	const suffix = checked ? "?additional_channels=nonguix" : "";
	if (homeLink) homeLink.setAttribute("href", "/" + suffix);
	if (statusLink) statusLink.setAttribute("href", "/status" + suffix);
}

function toggleMainNonguix(checked) {
	const cGuix = document.getElementById("main-commit-guix");
	const cNg = document.getElementById("main-commit-nonguix");
	const cliGuix = document.getElementById("main-cli-guix");
	const cliNg = document.getElementById("main-cli-nonguix");
	if (cGuix && cNg) {
		cGuix.style.display = checked ? "none" : "block";
		cNg.style.display = checked ? "block" : "none";
	}
	if (cliGuix && cliNg) {
		cliGuix.style.display = checked ? "none" : "flex";
		cliNg.style.display = checked ? "flex" : "none";
	}
	syncNonguixURLParam(checked);
}

function toggleStatusNonguix(checked) {
	const cGuix = document.getElementById("status-commit-guix");
	const cNg = document.getElementById("status-commit-nonguix");
	if (cGuix && cNg) {
		cGuix.style.display = checked ? "none" : "block";
		cNg.style.display = checked ? "block" : "none";
	}
	document.querySelectorAll(".col-nonguix").forEach((el) => {
		el.style.display = checked ? "" : "none";
	});
	syncNonguixURLParam(checked);
}
`)

	if includeFinder {
		b.WriteString(`
async function runFinder(ev) {
	ev.preventDefault();
	const out = document.getElementById("finder-result");
	out.style.display = "block";
	out.innerHTML = '<span class="badge badge-warn">Resolving...</span>';

	const pkgs = document.getElementById("f-pkgs").value.trim();
	const maxPending = document.getElementById("f-max-pending").value || "0";
	const reqNonguix = document.getElementById("f-additional-nonguix") && document.getElementById("f-additional-nonguix").checked;
	const allowFailed = document.getElementById("f-allow-failed").checked;

	const params = new URLSearchParams();
	params.set("system", "x86_64-linux");
	if (pkgs) {
		params.set("pkgs", pkgs);
	} else {
		params.set("max_pending", maxPending);
	}
	if (reqNonguix) params.set("additional_channels", "nonguix");
	if (allowFailed) params.set("allow_failed", "true");

	try {
		const [statusResp, scmResp] = await Promise.all([
			fetch("/status?" + params.toString(), {
				headers: { "Accept": "application/json" }
			}),
			fetch("/channels.scm?" + params.toString())
		]);
		const data = await statusResp.json();
		if (!statusResp.ok) {
			out.innerHTML = '<div class="badge badge-err">Error: ' + escapeHtml(data.error || statusResp.statusText) + '</div>';
			return;
		}
		const scmText = await scmResp.text();
		let untrackedHtml = "";
		if (data.untracked_packages && data.untracked_packages.length > 0) {
			untrackedHtml = '<div style="margin-top:0.4rem;font-size:0.82rem;color:var(--warn-text);">Skipped untracked/local packages: <code>' +
				escapeHtml(data.untracked_packages.join(", ")) + '</code></div>';
		}
		let addChHtml = "";
		if (data.additional_channels && data.additional_channels.length > 0) {
			data.additional_channels.forEach((ch) => {
				addChHtml += '<div style="margin-top:0.35rem;"><strong>Resolved ' + escapeHtml(ch.channel) + ' Commit:</strong> ' +
					'<code class="mono">' + escapeHtml(ch.commit) + '</code> ' +
					'<span class="badge badge-ok">Eval #' + ch.evaluation_id + ' (' + ch.evaluations_behind + ' behind)</span></div>';
			});
		}
		const channelsQueryUrl = window.location.origin + "/channels.scm?" + params.toString();
		const guixPullCmd = 'guix pull -C <(curl -fsSL "' + channelsQueryUrl + '")';
		const downloadedChannelsExpr = '(downloaded-channels\n  "' + channelsQueryUrl + '")';
		out.innerHTML =
			'<div style="padding:0.85rem;background:var(--code-bg);border:1px solid var(--border);border-radius:6px;">' +
				'<div style="display:flex;flex-wrap:wrap;justify-content:space-between;align-items:center;gap:0.5rem;">' +
					'<div>' +
						'<div><strong>Resolved guix Commit:</strong> <code class="mono">' + escapeHtml(data.commit) + '</code> ' +
						'<span class="badge badge-ok">Eval #' + data.evaluation_id + ' (' + data.evaluations_behind + ' behind)</span></div>' +
						addChHtml +
					'</div>' +
					'<div class="btn-row">' +
						'<button type="button" data-copy="' + escapeHtml(data.commit) + '" onclick="copyFromBtn(this)">Copy Commit</button>' +
						'<button type="button" data-copy="' + escapeHtml(guixPullCmd) + '" onclick="copyFromBtn(this)">Copy guix pull</button>' +
						'<button type="button" data-copy="' + escapeHtml(scmText) + '" onclick="copyFromBtn(this)">Copy channels.scm</button>' +
					'</div>' +
				'</div>' +
				untrackedHtml +
				'<pre class="code-box" style="background:var(--bg);">' + escapeHtml(scmText) + '</pre>' +
				'<div style="margin-top:0.6rem;font-size:0.85rem;">' +
					'<div style="margin-bottom:0.25rem;color:var(--text-muted);">Or track automatically in <code>~/.config/guix/channels.scm</code> via <code>downloaded-channels</code>:</div>' +
					'<pre class="code-box" style="background:var(--bg);">' + escapeHtml(downloadedChannelsExpr) + '</pre>' +
				'</div>' +
			'</div>';
	} catch (err) {
		out.innerHTML = '<div class="badge badge-err">Request failed: ' + escapeHtml(String(err)) + '</div>';
	}
}
`)
	}

	b.WriteString(`</script>
</body>
</html>
`)
	return b.String()
}

func shortCommit(commit string) string {
	if len(commit) > 10 {
		return commit[:10]
	}
	return commit
}
