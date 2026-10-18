package api

import (
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"

	"guix-evaluated/internal/poller"
	"guix-evaluated/internal/resolver"
	"guix-evaluated/internal/store"
)

type systemOverview struct {
	System        string                  `json:"system"`
	StrictReady   *resolver.Result        `json:"strict_ready,omitempty"`
	BestAvailable *resolver.Result        `json:"best_available,omitempty"`
	LatestEval    *evaluationSummaryItem  `json:"latest_eval,omitempty"`
	Evaluations   []evaluationSummaryItem `json:"evaluations"`
}

type dashboardState struct {
	BaseURL       string                    `json:"base_url"`
	ServerURL     string                    `json:"server_url"`
	ActiveSystem  string                    `json:"active_system"`
	Systems       []string                  `json:"systems"`
	Syncing       bool                      `json:"syncing"`
	LastSyncedAt  time.Time                 `json:"last_synced_at"`
	LastPollError string                    `json:"last_poll_error,omitempty"`
	TotalEvals    int                       `json:"total_evaluations"`
	BySystem      map[string]systemOverview `json:"by_system"`
}

func buildDashboardState(baseURL string, snap *store.Snapshot, p *poller.Poller, requestedSystem string) dashboardState {
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

	var bestIdx = -1
	var minPending = -1

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

func renderPlainIndex(st dashboardState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "guix-sync-commit service\n")
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
	fmt.Fprintf(&b, "  guix pull --commit=$(curl -fsSL \"%s/v1/commit\")\n\n", st.BaseURL)
	fmt.Fprintf(&b, "  # 2. Generate ~/.config/guix/channels.scm:\n")
	fmt.Fprintf(&b, "  curl -fsSL \"%s/v1/channels.scm\" > ~/.config/guix/channels.scm\n\n", st.BaseURL)
	fmt.Fprintf(&b, "  # 3. Inspect detailed JSON status:\n")
	fmt.Fprintf(&b, "  curl -fsSL \"%s/v1/status\"\n", st.BaseURL)
	return b.String()
}

func renderHTMLDashboard(st dashboardState) string {
	var b strings.Builder
	ov := st.BySystem[st.ActiveSystem]
	escBaseURL := html.EscapeString(st.BaseURL)
	escServerURL := html.EscapeString(st.ServerURL)

	b.WriteString(renderPageHead("Guix Pull with All Prebuilts Ready"))
	b.WriteString(renderPageHeader("main"))

	b.WriteString(`<main>`)
	b.WriteString(renderReadyCommitBlock(ov, escServerURL))
	b.WriteString(renderCLIExamplesBlock(ov, escBaseURL))
	b.WriteString(`</main>`)

	b.WriteString(renderFooter(st))
	b.WriteString(renderCommonScripts(st, false))
	return b.String()
}

func renderHTMLStatusPage(st dashboardState) string {
	var b strings.Builder
	ov := st.BySystem[st.ActiveSystem]
	escServerURL := html.EscapeString(st.ServerURL)

	defaultMaxPending := 0
	if ov.StrictReady == nil && ov.BestAvailable != nil {
		defaultMaxPending = ov.BestAvailable.PendingCount
	}

	b.WriteString(renderPageHead("Guix Pull with All Prebuilts Ready - Status"))
	b.WriteString(renderPageHeader("status"))

	b.WriteString(`<main>`)
	b.WriteString(`<div class="cards">`)
	b.WriteString(renderReadyCommitCardCompact(ov, escServerURL))
	b.WriteString(renderLatestEvalCard(ov, escServerURL))
	b.WriteString(`</div>`)

	b.WriteString(renderInteractiveResolverPanel(defaultMaxPending))
	b.WriteString(renderEvaluationsTable(ov, escServerURL))
	b.WriteString(`</main>`)

	b.WriteString(renderFooter(st))
	b.WriteString(renderCommonScripts(st, true))
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

  /* Site Header (Styled like whhone.com) */
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
  .examples-title {
    font-size: 0.82rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--muted);
    font-weight: 600;
    margin: 0;
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

func renderPageHeader(currentTab string) string {
	statusClass := ""
	if currentTab == "status" {
		statusClass = " class=\"active\""
	}

	return fmt.Sprintf(`
  <header class="site-header-container">
    <strong class="site-header-element-left"><a href="/">Guix Pull with All Prebuilts Ready</a></strong>
    <nav class="site-header-element-right">
      <a href="/status"%s>Status</a>
    </nav>
  </header>
`, statusClass)
}

func renderReadyCommitBlock(ov systemOverview, escServerURL string) string {
	var b strings.Builder
	b.WriteString(`<div class="panel">`)
	if ov.StrictReady != nil {
		r := ov.StrictReady
		dateHTML := ""
		if !r.EvaluationDate.IsZero() {
			utcStr := r.EvaluationDate.UTC().Format("2006-01-02 15:04:05 UTC")
			shortUTC := r.EvaluationDate.UTC().Format("2006-01-02 15:04")
			isoStr := r.EvaluationDate.UTC().Format(time.RFC3339)
			dateHTML = fmt.Sprintf(` &middot; <span class="local-time" data-utc="%s" title="%s">%s</span>`,
				html.EscapeString(isoStr), html.EscapeString(utcStr), html.EscapeString(shortUTC))
		}
		fmt.Fprintf(&b, `
    <h2>Latest Commit with All Prebuilts Ready on Substitutes</h2>
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
		dateHTML := ""
		if !r.EvaluationDate.IsZero() {
			utcStr := r.EvaluationDate.UTC().Format("2006-01-02 15:04:05 UTC")
			shortUTC := r.EvaluationDate.UTC().Format("2006-01-02 15:04")
			isoStr := r.EvaluationDate.UTC().Format(time.RFC3339)
			dateHTML = fmt.Sprintf(` &middot; <span class="local-time" data-utc="%s" title="%s">%s</span>`,
				html.EscapeString(isoStr), html.EscapeString(utcStr), html.EscapeString(shortUTC))
		}
		fmt.Fprintf(&b, `
    <div style="display:flex;justify-content:space-between;align-items:center;flex-wrap:wrap;gap:0.5rem;margin-bottom:0.35rem;">
      <h2 style="margin:0;">Closest to Ready (%d Pending)</h2>
      <span class="badge badge-warn">No 0-pending in last %d</span>
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
		b.WriteString(`
    <h2>Latest Commit with All Prebuilts Ready on Substitutes</h2>
    <div class="card-meta">Initial evaluation sync from Cuirass is in progress.</div>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

func renderCLIExamplesBlock(ov systemOverview, escBaseURL string) string {
	var b strings.Builder
	b.WriteString(`<div class="panel">`)
	b.WriteString(`<h2>Usages</h2>`)
	b.WriteString(`<div class="examples-list">`)

	readyCommit := ""
	if ov.StrictReady != nil {
		readyCommit = ov.StrictReady.Commit
	} else if ov.BestAvailable != nil {
		readyCommit = ov.BestAvailable.Commit
	}

	if readyCommit != "" {
		fmt.Fprintf(&b, `
      <div class="example-item">
        <div class="example-desc">Update Guix directly to this evaluated commit:</div>
        <div class="code-box-row">
          <pre class="mono">guix pull --commit=%s</pre>
          <button type="button" class="btn" data-copy="guix pull --commit=%s" onclick="copyFromBtn(this)">Copy</button>
        </div>
      </div>`,
			html.EscapeString(readyCommit), html.EscapeString(readyCommit),
		)
	}

	fmt.Fprintf(&b, `
      <div class="example-item">
        <div class="example-desc">Pull the newest ready commit dynamically via API:</div>
        <div class="code-box-row">
          <pre class="mono">guix pull --commit=$(curl -fsSL "%s/v1/commit")</pre>
          <button type="button" class="btn" data-copy="guix pull --commit=$(curl -fsSL &quot;%s/v1/commit&quot;)" onclick="copyFromBtn(this)">Copy</button>
        </div>
      </div>
      <div class="example-item">
        <div class="example-desc">Generate ~/.config/guix/channels.scm:</div>
        <div class="code-box-row">
          <pre class="mono">curl -fsSL "%s/v1/channels.scm" &gt; ~/.config/guix/channels.scm</pre>
          <button type="button" class="btn" data-copy="curl -fsSL &quot;%s/v1/channels.scm&quot; &gt; ~/.config/guix/channels.scm" onclick="copyFromBtn(this)">Copy</button>
        </div>
      </div>
    </div>
  </div>`,
		escBaseURL, escBaseURL,
		escBaseURL, escBaseURL,
	)
	return b.String()
}

func renderReadyCommitCardCompact(ov systemOverview, escServerURL string) string {
	var b strings.Builder
	b.WriteString(`<div class="card">`)
	if ov.StrictReady != nil {
		r := ov.StrictReady
		short := shortCommit(r.Commit)
		dateHTML := ""
		if !r.EvaluationDate.IsZero() {
			utcStr := r.EvaluationDate.UTC().Format("2006-01-02 15:04:05 UTC")
			shortUTC := r.EvaluationDate.UTC().Format("2006-01-02 15:04")
			isoStr := r.EvaluationDate.UTC().Format(time.RFC3339)
			dateHTML = fmt.Sprintf(` &middot; <span class="local-time" data-utc="%s" title="%s">%s</span>`,
				html.EscapeString(isoStr), html.EscapeString(utcStr), html.EscapeString(shortUTC))
		}
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
		dateHTML := ""
		if !r.EvaluationDate.IsZero() {
			utcStr := r.EvaluationDate.UTC().Format("2006-01-02 15:04:05 UTC")
			shortUTC := r.EvaluationDate.UTC().Format("2006-01-02 15:04")
			isoStr := r.EvaluationDate.UTC().Format(time.RFC3339)
			dateHTML = fmt.Sprintf(` &middot; <span class="local-time" data-utc="%s" title="%s">%s</span>`,
				html.EscapeString(isoStr), html.EscapeString(utcStr), html.EscapeString(shortUTC))
		}
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

func renderInteractiveResolverPanel(defaultMaxPending int) string {
	return fmt.Sprintf(`
  <div class="panel">
    <h2>Interactive Commit &amp; Package Resolver</h2>
    <p class="sub">Find the newest commit where your specific packages (or the whole evaluation) have substitutes ready.</p>
    <form id="finder-form" onsubmit="runFinder(event)">
      <div class="form-grid">
        <div class="field">
          <label for="f-pkgs">Packages (comma or space separated, leave empty for whole-evaluation mode)</label>
          <input type="text" id="f-pkgs" name="pkgs" class="mono" placeholder="e.g. emacs, git, linux-libre">
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
        <label><input type="checkbox" id="f-require-pull"> Require prebuilt <code>guix pull</code> binary</label>
        <label><input type="checkbox" id="f-allow-failed"> Allow failed builds in package mode</label>
      </div>
    </form>
    <div id="finder-result"></div>
  </div>
`, defaultMaxPending)
}

func renderEvaluationsTable(ov systemOverview, escServerURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `
  <div class="panel">
    <h2>Recent Master Evaluations</h2>
    <p class="sub">Showing %d cached evaluations ordered from newest to oldest.</p>
`, len(ov.Evaluations))

	if len(ov.Evaluations) == 0 {
		b.WriteString(`<p>No evaluations cached yet. Background sync is running...</p>`)
	} else {
		b.WriteString(`
    <div class="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Eval</th>
            <th>Commit</th>
            <th>Date (Local)</th>
            <th class="prog-cell">Build Progress</th>
            <th>guix pull</th>
            <th>Pending Sample</th>
          </tr>
        </thead>
        <tbody>
`)
		for idx, item := range ov.Evaluations {
			short := shortCommit(item.Commit)
			dateCell := `<td class="mono" style="white-space:nowrap;">unknown</td>`
			if !item.Timestamp.IsZero() {
				utcStr := item.Timestamp.UTC().Format("2006-01-02 15:04:05 UTC")
				shortUTC := item.Timestamp.UTC().Format("2006-01-02 15:04")
				isoStr := item.Timestamp.UTC().Format(time.RFC3339)
				dateCell = fmt.Sprintf(`<td class="mono local-time" style="white-space:nowrap;" data-utc="%s" title="%s">%s</td>`,
					html.EscapeString(isoStr), html.EscapeString(utcStr), html.EscapeString(shortUTC))
			}

			okPct, warnPct, errPct := 0.0, 0.0, 0.0
			if item.TotalJobs > 0 {
				okPct = float64(item.SucceededJobs) * 100.0 / float64(item.TotalJobs)
				warnPct = float64(item.PendingJobs) * 100.0 / float64(item.TotalJobs)
				errPct = float64(item.FailedJobs) * 100.0 / float64(item.TotalJobs)
			}

			pullCell := `<span class="badge badge-warn">Pending</span>`
			if item.PullReady {
				if item.PullEvalID > 0 {
					pullCell = fmt.Sprintf(`<a href="%s/eval/%d" target="_blank" rel="noopener"><span class="badge badge-ok">Ready (#%d)</span></a>`,
						escServerURL, item.PullEvalID, item.PullEvalID)
				} else {
					pullCell = `<span class="badge badge-ok">Ready</span>`
				}
			}

			var sampleHTML strings.Builder
			if item.PendingJobs == 0 {
				sampleHTML.WriteString(`<span class="badge badge-ok">All builds finished</span>`)
			} else if len(item.PendingSample) > 0 {
				sampleHTML.WriteString(`<div class="pending-tags">`)
				for _, jobName := range item.PendingSample {
					trimmed := strings.TrimSuffix(jobName, ".x86_64-linux")
					fmt.Fprintf(&sampleHTML, `<span class="pkg-tag mono" title="%s">%s</span>`,
						html.EscapeString(jobName), html.EscapeString(trimmed))
				}
				if item.PendingJobs > len(item.PendingSample) {
					fmt.Fprintf(&sampleHTML, `<span class="pkg-tag mono">+%d more</span>`,
						item.PendingJobs-len(item.PendingSample))
				}
				sampleHTML.WriteString(`</div>`)
			} else {
				fmt.Fprintf(&sampleHTML, `<span class="mono">%d pending</span>`, item.PendingJobs)
			}

			fmt.Fprintf(&b, `
          <tr>
            <td class="mono">
              <a href="%s/eval/%d" target="_blank" rel="noopener"><strong>#%d</strong></a>
              <div style="font-size:0.74rem;color:var(--muted);">lag: %d</div>
            </td>
            <td class="mono">
              <a href="https://codeberg.org/guix/guix/commit/%s" target="_blank" rel="noopener">%s</a>
              <button type="button" data-copy="%s" onclick="copyFromBtn(this)" style="padding:0.1rem 0.4rem;font-size:0.72rem;margin-left:0.3rem;">Copy</button>
            </td>
            %s
            <td class="prog-cell">
              <div class="prog-bar">
                <div class="prog-ok" style="width:%.2f%%"></div>
                <div class="prog-warn" style="width:%.2f%%"></div>
                <div class="prog-err" style="width:%.2f%%"></div>
              </div>
              <div class="prog-legend mono">
                <span style="color:var(--ok-text)">%d ok</span>
                <span style="color:var(--warn-text)">%d pend</span>
                <span style="color:var(--err-text)">%d fail</span>
              </div>
            </td>
            <td>%s</td>
            <td>%s</td>
          </tr>`,
				escServerURL, item.EvaluationID, item.EvaluationID, idx,
				html.EscapeString(item.Commit), html.EscapeString(short), html.EscapeString(item.Commit),
				dateCell,
				okPct, warnPct, errPct,
				item.SucceededJobs, item.PendingJobs, item.FailedJobs,
				pullCell,
				sampleHTML.String(),
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
      <a href="https://codeberg.org/guix/guix" target="_blank" rel="noopener">GNU Guix Source</a>
    </div>
  </footer>
`, escServerURL, escServerURL, lastSyncHTML)
}

func renderCommonScripts(st dashboardState, includeFinder bool) string {
	var b strings.Builder
	stateJSON, _ := json.Marshal(st)

	fmt.Fprintf(&b, `
<script>
const DASHBOARD_STATE = %s;
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
`, string(stateJSON))

	if includeFinder {
		b.WriteString(`
async function runFinder(ev) {
  ev.preventDefault();
  const out = document.getElementById("finder-result");
  out.style.display = "block";
  out.innerHTML = '<span class="badge badge-warn">Resolving...</span>';

  const pkgs = document.getElementById("f-pkgs").value.trim();
  const maxPending = document.getElementById("f-max-pending").value || "0";
  const reqPull = document.getElementById("f-require-pull").checked;
  const allowFailed = document.getElementById("f-allow-failed").checked;

  const params = new URLSearchParams();
  params.set("system", "x86_64-linux");
  if (pkgs) {
    params.set("pkgs", pkgs);
  } else {
    params.set("max_pending", maxPending);
  }
  if (reqPull) params.set("require_pull", "true");
  if (allowFailed) params.set("allow_failed", "true");

  try {
    const [statusResp, scmResp] = await Promise.all([
      fetch("/v1/status?" + params.toString()),
      fetch("/v1/channels.scm?" + params.toString())
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
    out.innerHTML =
      '<div style="padding:0.85rem;background:var(--code-bg);border:1px solid var(--border);border-radius:6px;">' +
        '<div style="display:flex;flex-wrap:wrap;justify-content:space-between;align-items:center;gap:0.5rem;">' +
          '<div><strong>Resolved Commit:</strong> <code class="mono">' + escapeHtml(data.commit) + '</code> ' +
          '<span class="badge badge-ok">Eval #' + data.evaluation_id + ' (' + data.evaluations_behind + ' behind)</span></div>' +
          '<div class="btn-row">' +
            '<button type="button" data-copy="' + escapeHtml(data.commit) + '" onclick="copyFromBtn(this)">Copy Commit</button>' +
            '<button type="button" data-copy="guix pull --commit=' + escapeHtml(data.commit) + '" onclick="copyFromBtn(this)">Copy guix pull</button>' +
          '</div>' +
        '</div>' +
        untrackedHtml +
        '<pre class="code-box" style="background:var(--bg);">' + escapeHtml(scmText) + '</pre>' +
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
