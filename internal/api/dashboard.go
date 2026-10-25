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

// renderHTMLDashboard renders the main landing page ("/") with a short service overview,
// CLI usage commands, and the recent master evaluations table.
func renderHTMLDashboard(st dashboardState) string {
	var b strings.Builder
	ov := st.BySystem[st.ActiveSystem]
	escBaseURL := html.EscapeString(st.BaseURL)
	escServerURL := html.EscapeString(st.ServerURL)

	b.WriteString(renderPageHead("Guix Pull with All Prebuilts Ready"))
	b.WriteString(renderPageHeader(st.RequireNonguix))

	b.WriteString(`<main>`)
	b.WriteString(renderIntroBlock())
	b.WriteString(renderCLIExamplesBlock(escBaseURL, st.RequireNonguix))
	b.WriteString(renderEvaluationsTable(ov, escServerURL, st.RequireNonguix))
	b.WriteString(`</main>`)

	b.WriteString(renderFooter(st))
	b.WriteString(renderCommonScripts())
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
		color-scheme: light dark;
		--bg: #ffffff;
		--surface: #ffffff;
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
		--btn-hover: #e2e8f0;
		--row-hover: #f8fafc;
	}
	@media (prefers-color-scheme: dark) {
		:root {
			--bg: #0f172a;
			--surface: #1e293b;
			--border: #334155;
			--text: #f1f5f9;
			--muted: #94a3b8;
			--accent: #60a5fa;
			--accent-hover: #3b82f6;
			--ok-bg: #064e3b;
			--ok-text: #6ee7b7;
			--ok-bar: #22c55e;
			--warn-bg: #451a03;
			--warn-text: #fcd34d;
			--warn-bar: #eab308;
			--err-bg: #450a0a;
			--err-text: #fca5a5;
			--err-bar: #ef4444;
			--code-bg: #0b1120;
			--btn-hover: #334155;
			--row-hover: #172033;
		}
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
		background: var(--surface);
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
	button:hover, .btn:hover { background: var(--btn-hover); }
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
		background: var(--code-bg);
		color: var(--text);
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
	tr:hover td { background: var(--row-hover); }
	tr.row-latest-ready td { background: rgba(34, 197, 94, 0.08); }
	tr.row-latest-ready:hover td { background: rgba(34, 197, 94, 0.14); }
	tr.row-latest-ready td:first-child { box-shadow: inset 3px 0 0 var(--ok-bar); }

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

func renderPageHeader(requireNonguix bool) string {
	querySuffix := ""
	if requireNonguix {
		querySuffix = "?additional_channels=nonguix"
	}

	return fmt.Sprintf(`
	<header class="site-header-container">
		<strong class="site-header-element-left"><a href="/%s">Guix Pull with All Prebuilts Ready</a></strong>
	</header>
`, querySuffix)
}

func renderIntroBlock() string {
	return `
	<div class="panel">
		<h2>About This Service</h2>
		<p style="margin:0;color:var(--muted);font-size:0.95rem;">
			<code>guix pull</code> to the newest commit where prebuilt binaries are 100% ready on substitutes servers, so updating your system never triggers slow local builds from source. Optionally coordinates with <a href="https://gitlab.com/nonguix/nonguix" target="_blank" rel="noopener">Nonguix</a>.
		</p>
	</div>
`
}

func renderCLIExamplesBlock(escBaseURL string, requireNonguix bool) string {
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
	b.WriteString(`<div style="display:flex;justify-content:space-between;align-items:center;flex-wrap:wrap;gap:0.5rem;">`)
	b.WriteString(`<h2 style="margin:0;">Usage</h2>`)
	fmt.Fprintf(&b, `<label style="font-size:0.88rem;font-weight:500;color:var(--muted);cursor:pointer;display:inline-flex;align-items:center;gap:0.35rem;"><input type="checkbox" id="main-nonguix-toggle"%s onchange="toggleMainNonguix(this.checked)"> Require <code>nonguix</code> channel</label>`, checkedAttr)
	b.WriteString(`</div>`)

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
				<div style="margin-top:0.35rem;font-size:0.8rem;color:var(--muted);">See GNU Guix manual on <a href="https://guix.gnu.org/manual/devel/en/html_node/Channels-with-Substitutes.html" target="_blank" rel="noopener">Channels with Substitutes</a>.</div>
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
				<div style="margin-top:0.35rem;font-size:0.8rem;color:var(--muted);">See GNU Guix manual on <a href="https://guix.gnu.org/manual/devel/en/html_node/Channels-with-Substitutes.html" target="_blank" rel="noopener">Channels with Substitutes</a>.</div>
			</div>
		</div>
	</div>`,
		ngStyle,
		escBaseURL, escBaseURL,
		escBaseURL, escBaseURL,
	)
	return b.String()
}

func renderEvaluationsTable(ov systemOverview, escServerURL string, requireNonguix bool) string {
	var b strings.Builder
	ngColStyle := ` style="display:none;"`
	if requireNonguix {
		ngColStyle = ""
	}
	var latestGuixEvalID int64
	if ov.StrictReady != nil {
		latestGuixEvalID = ov.StrictReady.EvaluationID
	}
	var latestNgEvalID int64
	if ov.NonguixStrictReady != nil {
		latestNgEvalID = ov.NonguixStrictReady.EvaluationID
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
			isLatestGuix := latestGuixEvalID > 0 && item.EvaluationID == latestGuixEvalID
			isLatestNg := latestNgEvalID > 0 && item.EvaluationID == latestNgEvalID

			var rowAttrs strings.Builder
			if isLatestGuix {
				rowAttrs.WriteString(` data-latest-guix="1"`)
			}
			if isLatestNg {
				rowAttrs.WriteString(` data-latest-nonguix="1"`)
			}
			if (!requireNonguix && isLatestGuix) || (requireNonguix && isLatestNg) {
				rowAttrs.WriteString(` class="row-latest-ready"`)
			}

			var latestBadges strings.Builder
			if isLatestGuix {
				badgeStyle := ` style="margin-left:0.35rem;"`
				if requireNonguix {
					badgeStyle = ` style="margin-left:0.35rem;display:none;"`
				}
				fmt.Fprintf(&latestBadges, `<span class="badge badge-ok latest-guix-badge"%s>Latest Ready</span>`, badgeStyle)
			}
			if isLatestNg {
				badgeStyle := ` style="margin-left:0.35rem;display:none;"`
				if requireNonguix {
					badgeStyle = ` style="margin-left:0.35rem;"`
				}
				fmt.Fprintf(&latestBadges, `<span class="badge badge-ok latest-nonguix-badge"%s>Latest Ready</span>`, badgeStyle)
			}

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
					<tr%s>
						<td class="mono">
							<a href="https://codeberg.org/guix/guix/commit/%s" target="_blank" rel="noopener">%s</a>
							<button type="button" data-copy="%s" onclick="copyFromBtn(this)" style="padding:0.1rem 0.4rem;font-size:0.72rem;margin-left:0.3rem;">Copy</button>%s
						</td>
						%s
						<td>%s</td>
						<td>%s</td>
						<td class="col-nonguix"%s>%s</td>
					</tr>`,
				rowAttrs.String(),
				html.EscapeString(item.Commit), html.EscapeString(short), html.EscapeString(item.Commit), latestBadges.String(),
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

func renderCommonScripts() string {
	return `
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
	const suffix = checked ? "?additional_channels=nonguix" : "";
	if (homeLink) homeLink.setAttribute("href", "/" + suffix);
}

function toggleMainNonguix(checked) {
	const cliGuix = document.getElementById("main-cli-guix");
	const cliNg = document.getElementById("main-cli-nonguix");
	if (cliGuix && cliNg) {
		cliGuix.style.display = checked ? "none" : "flex";
		cliNg.style.display = checked ? "flex" : "none";
	}
	document.querySelectorAll(".col-nonguix").forEach((el) => {
		el.style.display = checked ? "" : "none";
	});
	document.querySelectorAll("tbody tr").forEach((tr) => {
		const isLatest = checked
			? tr.getAttribute("data-latest-nonguix") === "1"
			: tr.getAttribute("data-latest-guix") === "1";
		tr.classList.toggle("row-latest-ready", isLatest);
	});
	document.querySelectorAll(".latest-guix-badge").forEach((el) => {
		el.style.display = checked ? "none" : "";
	});
	document.querySelectorAll(".latest-nonguix-badge").forEach((el) => {
		el.style.display = checked ? "" : "none";
	});
	syncNonguixURLParam(checked);
}
</script>
</body>
</html>
`
}

func shortCommit(commit string) string {
	if len(commit) > 10 {
		return commit[:10]
	}
	return commit
}
