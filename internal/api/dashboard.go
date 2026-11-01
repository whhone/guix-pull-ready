package api

import (
	_ "embed"
	"fmt"
	"html"
	"strings"
	"time"

	"guix-pull-ready/internal/cuirass"
	"guix-pull-ready/internal/resolver"
	"guix-pull-ready/internal/store"
)

//go:embed static/dashboard.css
var dashboardCSS string

//go:embed static/dashboard.js
var dashboardJS string

// channelEvalSummary holds the evaluation and pending build state for an additional channel
// coordinated with a primary Guix commit.
type channelEvalSummary struct {
	Channel       string
	EvalID        int64
	Commit        string
	Ready         bool
	PendingJobs   int
	PendingBuilds []store.PendingBuild
	PendingSample []string
	ServerURL     string
}

type evaluationSummaryItem struct {
	EvaluationID       int64
	Commit             string
	Timestamp          time.Time
	PendingJobs        int
	PullReady          bool
	PullEvalID         int64
	AdditionalChannels map[string]channelEvalSummary
}

type systemOverview struct {
	System             string
	StrictReady        *resolver.Result
	ChannelStrictReady map[string]*resolver.Result
	Evaluations        []evaluationSummaryItem
}

type dashboardState struct {
	BaseURL        string
	ServerURL      string
	ActiveSystem   string
	RequireNonguix bool
	Systems        []string
	LastSyncedAt   time.Time
	BySystem       map[string]systemOverview
}

// buildDashboardState assembles the view state across all tracked architectures
// for the HTML dashboard.
func buildDashboardState(baseURL string, snap *store.Snapshot, reqSystem string) dashboardState {
	defaultSystems := cuirass.DefaultSystems
	st := dashboardState{
		BaseURL:      baseURL,
		ServerURL:    "https://ci.guix.gnu.org",
		ActiveSystem: defaultSystems[0],
		Systems:      append([]string(nil), defaultSystems...),
		BySystem:     make(map[string]systemOverview),
	}

	if snap != nil {
		st.ServerURL = snap.ServerURL
		st.LastSyncedAt = snap.LastSyncedAt
		var systems []string
		if len(snap.Systems) > 0 {
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
		st.Systems = systems

		for _, sys := range st.Systems {
			st.BySystem[sys] = buildSystemOverview(snap, sys, 40)
		}
	}

	if reqSystem != "" {
		for _, sys := range st.Systems {
			if sys == reqSystem {
				st.ActiveSystem = reqSystem
				return st
			}
		}
	}

	hasDefault := false
	for _, sys := range st.Systems {
		if sys == "x86_64-linux" {
			hasDefault = true
			break
		}
	}
	if !hasDefault && len(st.Systems) > 0 {
		st.ActiveSystem = st.Systems[0]
	}

	return st
}

// buildSystemOverview summarizes up to limit recent evaluations for a target architecture.
func buildSystemOverview(snap *store.Snapshot, sys string, limit int) systemOverview {
	ov := systemOverview{
		System:             sys,
		ChannelStrictReady: make(map[string]*resolver.Result, len(resolver.SupportedAdditionalChannels)),
		Evaluations:        make([]evaluationSummaryItem, 0, limit),
	}
	if snap == nil {
		return ov
	}

	if strict, err := resolver.Resolve(snap, resolver.Query{System: sys, MaxPending: 0, Limit: limit}); err == nil {
		ov.StrictReady = strict
	}
	for _, ch := range resolver.SupportedAdditionalChannels {
		if chStrict, err := resolver.Resolve(snap, resolver.Query{
			System:             sys,
			MaxPending:         0,
			Limit:              limit,
			AdditionalChannels: []string{ch},
		}); err == nil {
			ov.ChannelStrictReady[ch] = chStrict
		}
	}

	for _, ev := range snap.Evaluations {
		data, ok := snap.ByEvalSystem[store.EvalSystemKey{EvalID: ev.ID, System: sys}]
		if !ok || data.TotalJobs == 0 {
			continue
		}

		item := evaluationSummaryItem{
			EvaluationID:       ev.ID,
			Commit:             ev.Commit,
			Timestamp:          ev.Timestamp,
			PendingJobs:        data.PendingJobs,
			PullReady:          data.PullReady,
			PullEvalID:         data.PullEvalID,
			AdditionalChannels: make(map[string]channelEvalSummary, len(resolver.SupportedAdditionalChannels)),
		}
		hasAdditionalChannelEval := false
		for _, ch := range resolver.SupportedAdditionalChannels {
			if chSum, ok := matchChannelEvaluation(snap, ch, ev.Commit, sys); ok {
				item.AdditionalChannels[ch] = chSum
				hasAdditionalChannelEval = true
			}
		}

		isStrictReady := ov.StrictReady != nil && ov.StrictReady.EvaluationID == ev.ID
		if !isStrictReady {
			for _, chRes := range ov.ChannelStrictReady {
				if chRes != nil && chRes.EvaluationID == ev.ID {
					isStrictReady = true
					break
				}
			}
		}
		if len(ov.Evaluations) >= limit && !hasAdditionalChannelEval && !isStrictReady {
			continue
		}
		ov.Evaluations = append(ov.Evaluations, item)
	}

	return ov
}

// matchChannelEvaluation finds the best evaluation for an additional channel paired with guixCommit on sys.
// It prefers a ready evaluation (PendingJobs == 0) over a pending one if multiple evaluations exist for the same Guix checkout.
func matchChannelEvaluation(snap *store.Snapshot, channel, guixCommit, sys string) (channelEvalSummary, bool) {
	if !cuirass.SupportsChannelJobset(channel, sys) {
		return channelEvalSummary{}, false
	}
	serverURL := snap.ChannelServerURLs[channel]
	if serverURL == "" {
		serverURL = cuirass.DefaultChannelServerURL(channel)
	}

	var matched channelEvalSummary
	for _, chEv := range snap.ChannelEvaluations[channel] {
		if chEv.Checkouts["guix"] != guixCommit {
			continue
		}
		chData, ok := snap.ByChannelEvalSystem[store.ChannelEvalSystemKey{
			Channel: channel,
			EvalID:  chEv.ID,
			System:  sys,
		}]
		if !ok || chData.TotalJobs == 0 {
			continue
		}
		if matched.EvalID == 0 || (!matched.Ready && chData.PendingJobs == 0) {
			matched = channelEvalSummary{
				Channel:       channel,
				EvalID:        chEv.ID,
				Commit:        chEv.Commit,
				Ready:         chData.PendingJobs == 0,
				PendingJobs:   chData.PendingJobs,
				PendingBuilds: chData.PendingBuilds,
				PendingSample: chData.PendingSample,
				ServerURL:     serverURL,
			}
		}
		if matched.Ready {
			break
		}
	}
	return matched, matched.EvalID > 0
}

// renderHTMLDashboard renders the main landing page ("/") with a short service overview,
// CLI usage commands, and the recent master evaluations table.
func renderHTMLDashboard(st dashboardState) string {
	var b strings.Builder
	ov := st.BySystem[st.ActiveSystem]
	escBaseURL := html.EscapeString(st.BaseURL)
	escServerURL := html.EscapeString(st.ServerURL)

	b.WriteString(renderPageHead("Guix Pull with All Prebuilts Ready"))
	b.WriteString(renderPageHeader(st))

	b.WriteString(`<main>`)
	b.WriteString(renderIntroBlock())
	b.WriteString(renderCLIExamplesBlock(escBaseURL, st.ActiveSystem, st.RequireNonguix))
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
<link rel="icon" type="image/svg+xml" href="/favicon.ico">
<style>
` + dashboardCSS + `
</style>
<noscript><style>.local-time[data-utc] { visibility: visible !important; }</style></noscript>
</head>
<body>
`
}

func buildQuerySuffix(activeSystem string, requireNonguix bool) string {
	var params []string
	if activeSystem != "" && activeSystem != "x86_64-linux" {
		params = append(params, "system="+activeSystem)
	}
	if requireNonguix {
		params = append(params, "additional_channels=nonguix")
	}
	if len(params) == 0 {
		return ""
	}
	return "?" + strings.Join(params, "&")
}

func renderPageHeader(st dashboardState) string {
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
		<strong class="site-header-element-left"><a href="/%s">Guix Pull with All Prebuilts Ready</a></strong>
		<div class="site-header-element-right">
			<label style="font-size:0.88rem;font-weight:500;color:var(--muted);cursor:pointer;display:inline-flex;align-items:center;gap:0.35rem;"><input type="checkbox" id="main-nonguix-toggle"%s onchange="toggleMainNonguix(this.checked)"> Require <code>nonguix</code> channel</label>
			<select id="arch-select" class="arch-select mono" aria-label="Target architecture" onchange="changeSystem(this.value)">%s</select>
		</div>
	</header>
`, querySuffix, checkedAttr, options.String())
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

func renderCLIExamplesBlock(escBaseURL string, activeSystem string, requireNonguix bool) string {
	var b strings.Builder
	guixStyle := ""
	ngStyle := ` style="display:none;"`
	if requireNonguix {
		guixStyle = ` style="display:none;"`
		ngStyle = ""
	}
	guixChannelsURL := escBaseURL + "/channels.scm" + html.EscapeString(buildQuerySuffix(activeSystem, false))
	ngChannelsURL := escBaseURL + "/channels.scm" + html.EscapeString(buildQuerySuffix(activeSystem, true))

	b.WriteString(`<div class="panel">`)
	b.WriteString(`<h2>Usage</h2>`)

	fmt.Fprintf(&b, `
		<div id="main-cli-guix" class="examples-list"%s>
			<div class="example-item">
				<div class="example-desc">Pull the newest ready commit dynamically via API:</div>
				<div class="code-box-row">
					<pre class="mono">guix pull -C &lt;(curl -fsSL "%s")</pre>
					<button type="button" class="btn" data-copy="guix pull -C &lt;(curl -fsSL &quot;%s&quot;)" onclick="copyFromBtn(this)">Copy</button>
				</div>
			</div>
			<div class="example-item">
				<div class="example-desc">Track automatically via ~/.config/guix/channels.scm:</div>
				<div class="code-box-row">
					<pre class="mono">(downloaded-channels "%s")</pre>
					<button type="button" class="btn" data-copy="(downloaded-channels &quot;%s&quot;)" onclick="copyFromBtn(this)">Copy</button>
				</div>
				<div style="margin-top:0.35rem;font-size:0.8rem;color:var(--muted);">See GNU Guix manual on <a href="https://guix.gnu.org/manual/devel/en/html_node/Channels-with-Substitutes.html" target="_blank" rel="noopener">Channels with Substitutes</a>.</div>
			</div>
		</div>`,
		guixStyle,
		guixChannelsURL, guixChannelsURL,
		guixChannelsURL, guixChannelsURL,
	)

	fmt.Fprintf(&b, `
		<div id="main-cli-nonguix" class="examples-list"%s>
			<div style="padding:0.65rem 0.85rem;background:var(--warn-bg);color:var(--warn-text);border-radius:6px;font-size:0.86rem;">
				<strong>IMPORTANT:</strong> <code>nonguix</code> substitutes require one-time manual setup. Follow <a href="https://gitlab.com/nonguix/nonguix/#substitutes-for-nonguix" target="_blank" rel="noopener" style="color:inherit;font-weight:600;text-decoration:underline;">Substitutes for Nonguix</a> to authorize the substitute server.
			</div>
			<div class="example-item">
				<div class="example-desc">Pull both guix and nonguix at their coordinated ready commits:</div>
				<div class="code-box-row">
					<pre class="mono">guix pull -C &lt;(curl -fsSL "%s")</pre>
					<button type="button" class="btn" data-copy="guix pull -C &lt;(curl -fsSL &quot;%s&quot;)" onclick="copyFromBtn(this)">Copy</button>
				</div>
			</div>
			<div class="example-item">
				<div class="example-desc">Track both channels automatically via ~/.config/guix/channels.scm:</div>
				<div class="code-box-row">
					<pre class="mono">(downloaded-channels "%s")</pre>
					<button type="button" class="btn" data-copy="(downloaded-channels &quot;%s&quot;)" onclick="copyFromBtn(this)">Copy</button>
				</div>
				<div style="margin-top:0.35rem;font-size:0.8rem;color:var(--muted);">See GNU Guix manual on <a href="https://guix.gnu.org/manual/devel/en/html_node/Channels-with-Substitutes.html" target="_blank" rel="noopener">Channels with Substitutes</a>.</div>
			</div>
		</div>
	</div>`,
		ngStyle,
		ngChannelsURL, ngChannelsURL,
		ngChannelsURL, ngChannelsURL,
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
	if ngReady := ov.ChannelStrictReady["nonguix"]; ngReady != nil {
		latestNgEvalID = ngReady.EvaluationID
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
						<th><a href="https://codeberg.org/guix/guix/commits/branch/master" target="_blank" rel="noopener">guix commit</a></th>
						<th>Committed</th>
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

			var pullCell string
			if !cuirass.SupportsGuixJobset(ov.System) {
				pullCell = fmt.Sprintf(`<span class="badge badge-muted" title="ci.guix.gnu.org does not build the guix jobset for %s">Not built</span>`,
					html.EscapeString(ov.System))
			} else {
				pullBadge := `<span class="badge badge-warn">Pending</span>`
				if item.PullReady {
					pullBadge = `<span class="badge badge-ok">Ready</span>`
				}
				pullCell = pullBadge
				if item.PullEvalID > 0 {
					pullCell = fmt.Sprintf(`<div style="display:inline-flex;align-items:center;gap:0.4rem;"><a href="%s/eval/%d" target="_blank" rel="noopener" class="mono"><strong>#%d</strong></a>%s</div>`,
						escServerURL, item.PullEvalID, item.PullEvalID, pullBadge)
				}
			}

			masterBadge := `<span class="badge badge-warn">Pending</span>`
			if item.PendingJobs == 0 {
				masterBadge = `<span class="badge badge-ok">Ready</span>`
			}
			masterCell := fmt.Sprintf(`<div style="display:inline-flex;align-items:center;gap:0.4rem;"><a href="%s/eval/%d" target="_blank" rel="noopener" class="mono"><strong>#%d</strong></a>%s</div>`,
				escServerURL, item.EvaluationID, item.EvaluationID, masterBadge)

			nonguixCell := renderChannelCell("nonguix", item, ov.System)

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

func renderChannelCell(channel string, item evaluationSummaryItem, sys string) string {
	if !cuirass.SupportsChannelJobset(channel, sys) {
		serverHost := strings.TrimPrefix(cuirass.DefaultChannelServerURL(channel), "https://")
		if serverHost == "" {
			serverHost = channel
		}
		return fmt.Sprintf(`<span class="badge badge-muted" title="%s does not build substitutes for %s">Not built</span>`,
			html.EscapeString(serverHost), html.EscapeString(sys))
	}
	chSum, ok := item.AdditionalChannels[channel]
	if !ok || chSum.EvalID == 0 {
		return fmt.Sprintf(`<span style="color:var(--muted);font-size:0.8rem;" title="No %s evaluation against this Guix commit">-</span>`,
			html.EscapeString(channel))
	}

	escServer := html.EscapeString(chSum.ServerURL)
	shortChCommit := html.EscapeString(shortCommit(chSum.Commit))
	escChannel := html.EscapeString(channel)
	if chSum.Ready {
		return fmt.Sprintf(`<div style="display:inline-flex;align-items:center;gap:0.4rem;"><a href="%s/eval/%d" target="_blank" rel="noopener" class="mono" title="%s commit %s"><strong>#%d</strong></a><span class="badge badge-ok">Ready</span></div>`,
			escServer, chSum.EvalID, escChannel, shortChCommit, chSum.EvalID)
	}

	builds := chSum.PendingBuilds
	if len(builds) == 0 && len(chSum.PendingSample) > 0 {
		builds = make([]store.PendingBuild, len(chSum.PendingSample))
		for i, name := range chSum.PendingSample {
			builds[i] = store.PendingBuild{JobName: name}
		}
	}

	var sampleNames []string
	for _, pb := range builds {
		if pb.JobName != "" {
			sampleNames = append(sampleNames, pb.JobName)
		}
	}
	var badgeTitleAttr string
	var tooltip string
	if len(sampleNames) > 0 {
		tooltip = "Pending jobs: " + strings.Join(sampleNames, ", ")
		if chSum.PendingJobs > len(sampleNames) {
			tooltip += fmt.Sprintf(" (+%d more)", chSum.PendingJobs-len(sampleNames))
		}
		badgeTitleAttr = fmt.Sprintf(` title="%s"`, html.EscapeString(tooltip))
	}

	badgeLabel := "Pending"
	if chSum.PendingJobs > 0 {
		badgeLabel = fmt.Sprintf("Pending (%d)", chSum.PendingJobs)
	}
	chBadge := fmt.Sprintf(`<span class="badge badge-warn"%s>%s</span>`, badgeTitleAttr, badgeLabel)

	var detailsHTML string
	if len(builds) > 0 {
		maxShow := 2
		if len(builds) < maxShow {
			maxShow = len(builds)
		}
		var parts []string
		for i := 0; i < maxShow; i++ {
			pb := builds[i]
			pkg := store.BasePackageFromJobName(pb.JobName, sys)
			if pkg == "" {
				pkg = pb.JobName
			}
			escPkg := html.EscapeString(pkg)
			escJob := html.EscapeString(pb.JobName)
			if pb.BuildID > 0 {
				parts = append(parts, fmt.Sprintf(`<a href="%s/build/%d/details" target="_blank" rel="noopener" class="mono" title="Pending build #%d (%s)">%s</a>`,
					escServer, pb.BuildID, pb.BuildID, escJob, escPkg))
			} else {
				parts = append(parts, fmt.Sprintf(`<span class="mono" title="Pending job %s">%s</span>`, escJob, escPkg))
			}
		}
		extra := ""
		if chSum.PendingJobs > maxShow {
			extra = fmt.Sprintf(` <span%s>+%d more</span>`, badgeTitleAttr, chSum.PendingJobs-maxShow)
		}
		detailsHTML = fmt.Sprintf(`<span style="font-size:0.78rem;color:var(--muted);">%s%s</span>`,
			strings.Join(parts, ", "), extra)
	}

	return fmt.Sprintf(`<div style="display:inline-flex;align-items:center;gap:0.4rem;flex-wrap:wrap;"><a href="%s/eval/%d" target="_blank" rel="noopener" class="mono" title="%s commit %s"><strong>#%d</strong></a>%s%s</div>`,
		escServer, chSum.EvalID, escChannel, shortChCommit, chSum.EvalID, chBadge, detailsHTML)
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
` + dashboardJS + `
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
