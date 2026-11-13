package api

import (
	_ "embed"
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

//go:embed static/dashboard.css
var dashboardCSS string

//go:embed static/dashboard.js
var dashboardJS string

// unavailablePackageBuild describes a single build of a multi-build package in an evaluation.
type unavailablePackageBuild struct {
	Version string `json:"version,omitempty"`
	Status  string `json:"status"`
	BuildID int64  `json:"build_id,omitempty"`
}

// unavailablePackageItem describes a package and its build status(es) in an evaluation.
type unavailablePackageItem struct {
	Name    string                    `json:"name"`
	Channel string                    `json:"channel"`
	Status  string                    `json:"status"`
	BuildID int64                     `json:"build_id,omitempty"`
	Builds  []unavailablePackageBuild `json:"builds,omitempty"`
}

type unavailableModePayload struct {
	Available        bool                     `json:"available"`
	Spec             string                   `json:"spec,omitempty"`
	Commit           string                   `json:"commit,omitempty"`
	EvaluationID     int64                    `json:"evaluation_id,omitempty"`
	NonguixCommit    string                   `json:"nonguix_commit,omitempty"`
	NonguixEvalID    int64                    `json:"nonguix_eval_id,omitempty"`
	ServerURL        string                   `json:"server_url"`
	NonguixURL       string                   `json:"nonguix_url,omitempty"`
	System           string                   `json:"system"`
	AvailableCount   int                      `json:"available_count"`
	UnavailableCount int                      `json:"unavailable_count"`
	Items            []unavailablePackageItem `json:"items"`
	Succeeded        []string                 `json:"succeeded,omitempty"`
	NonguixSucceeded []string                 `json:"nonguix_succeeded,omitempty"`
}

type unavailableDashboardPayload struct {
	Guix    unavailableModePayload `json:"guix"`
	Nonguix unavailableModePayload `json:"nonguix"`
}

// channelEvalSummary holds the evaluation and pending build state for an additional channel
// coordinated with a primary Guix commit.
type channelEvalSummary struct {
	Channel       string
	EvalID        int64
	Commit        string
	Ready         bool
	PendingJobs   int
	FailedJobs    int
	PendingBuilds []store.PendingBuild
	PendingSample []string
	ServerURL     string
}

type evaluationSummaryItem struct {
	EvaluationID       int64
	Commit             string
	Timestamp          time.Time
	PendingJobs        int
	FailedJobs         int
	PullReady          bool
	PullEvalID         int64
	PullPendingJobs    int
	PullFailedJobs     int
	AdditionalChannels map[string]channelEvalSummary
}

type systemOverview struct {
	System             string
	StrictReady        *resolver.Result
	ChannelStrictReady map[string]*resolver.Result
	ChannelServerURLs  map[string]string
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
		ChannelServerURLs:  make(map[string]string, len(resolver.SupportedAdditionalChannels)),
		Evaluations:        make([]evaluationSummaryItem, 0, limit),
	}
	if snap == nil {
		return ov
	}

	for _, ch := range resolver.SupportedAdditionalChannels {
		serverURL := snap.ChannelServerURLs[ch]
		if serverURL == "" {
			serverURL = cuirass.DefaultChannelServerURL(ch)
		}
		ov.ChannelServerURLs[ch] = serverURL
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
			FailedJobs:         data.FailedJobs,
			PullReady:          data.PullReady,
			PullEvalID:         data.PullEvalID,
			AdditionalChannels: make(map[string]channelEvalSummary, len(resolver.SupportedAdditionalChannels)),
		}
		if data.PullEvalID > 0 {
			if pullData := snap.ByChannelEvalSystem[store.ChannelEvalSystemKey{
				Channel: store.GuixJobsetChannel,
				EvalID:  data.PullEvalID,
				System:  sys,
			}]; pullData != nil {
				item.PullPendingJobs = pullData.PendingJobs
				item.PullFailedJobs = pullData.FailedJobs
			}
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

// collectEvaluationPackages extracts all packages (both succeeded and unavailable) in a given evaluation,
// returning:
//   - allItems: every package (succeeded and unavailable) sorted by name
//   - detailedItems: non-succeeded packages + multi-version succeeded packages (with BuildID)
//   - singleSucceeded: sorted names of single-version succeeded packages (for compact JSON encoding)
func collectEvaluationPackages(snap *store.Snapshot, channel string, evalID int64, sys string) (allItems []unavailablePackageItem, detailedItems []unavailablePackageItem, singleSucceeded []string) {
	if snap == nil || evalID <= 0 {
		return nil, nil, nil
	}

	var data *store.SystemEvalData
	var ok bool
	displayChannel := channel

	if channel == "" || channel == "guix" {
		channel = "guix"
		displayChannel = "guix"
		data, ok = snap.ByEvalSystem[store.EvalSystemKey{EvalID: evalID, System: sys}]
	} else {
		if channel == store.GuixJobsetChannel {
			displayChannel = "guix"
		}
		data, ok = snap.ByChannelEvalSystem[store.ChannelEvalSystemKey{
			Channel: channel,
			EvalID:  evalID,
			System:  sys,
		}]
	}
	if !ok || data == nil {
		return nil, nil, nil
	}

	snap.ForEachSingleJob(data, func(base string, st cuirass.JobStatus) {
		item := unavailablePackageItem{
			Name:    base,
			Channel: displayChannel,
			Status:  st.String(),
		}
		if st == cuirass.StatusSucceeded {
			singleSucceeded = append(singleSucceeded, base)
		} else {
			item.BuildID = findEvalBuildID(data, base, sys)
			detailedItems = append(detailedItems, item)
		}
		allItems = append(allItems, item)
	})

	for base, jobs := range data.JobsByBase {
		if len(jobs) == 0 {
			continue
		}
		sortedJobs := append([]cuirass.Job(nil), jobs...)
		sort.SliceStable(sortedJobs, func(i, j int) bool {
			if sortedJobs[i].Version != sortedJobs[j].Version {
				if sortedJobs[i].Version == "" {
					return false
				}
				if sortedJobs[j].Version == "" {
					return true
				}
				return cuirass.VersionGreater(sortedJobs[i].Version, sortedJobs[j].Version)
			}
			return sortedJobs[i].BuildID > sortedJobs[j].BuildID
		})
		builds := make([]unavailablePackageBuild, 0, len(sortedJobs))
		for _, job := range sortedJobs {
			builds = append(builds, unavailablePackageBuild{
				Version: job.Version,
				Status:  job.Status.String(),
				BuildID: job.BuildID,
			})
		}
		item := unavailablePackageItem{
			Name:    base,
			Channel: displayChannel,
			Status:  sortedJobs[0].Status.String(),
			BuildID: sortedJobs[0].BuildID,
			Builds:  builds,
		}
		allItems = append(allItems, item)
		detailedItems = append(detailedItems, item)
	}

	if len(allItems) > 0 {
		snap.ForEachMissingTrackedPackage(channel, sys, data, func(base string) {
			item := unavailablePackageItem{
				Name:    base,
				Channel: displayChannel,
				Status:  "not-built",
			}
			allItems = append(allItems, item)
			detailedItems = append(detailedItems, item)
		})
	}

	sortUnavailablePackages(allItems)
	sortUnavailablePackages(detailedItems)
	sort.Strings(singleSucceeded)
	return allItems, detailedItems, singleSucceeded
}

func sortUnavailablePackages(items []unavailablePackageItem) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		return items[i].Channel < items[j].Channel
	})
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
				FailedJobs:    chData.FailedJobs,
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

	b.WriteString(renderPageHead("Guix Pull Ready"))
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
		<strong class="site-header-element-left"><a href="/%s">Guix Pull Ready</a></strong>
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

func renderStatusBadgeHTML(status string) string {
	switch status {
	case "succeeded":
		return `<span class="badge badge-ok">Succeeded</span>`
	case "failed":
		return `<span class="badge badge-danger">Failed</span>`
	case "failed-dependency":
		return `<span class="badge badge-warn">Failed (dependency)</span>`
	case "failed-other":
		return `<span class="badge badge-warn">Failed (other)</span>`
	case "scheduled", "submitted", "started":
		return `<span class="badge badge-warn">Pending</span>`
	case "stuck":
		return `<span class="badge badge-warn">Stuck</span>`
	case "canceled":
		return `<span class="badge badge-muted">Canceled</span>`
	case "not-built":
		return `<span class="badge badge-muted">Not built</span>`
	default:
		return `<span class="badge badge-warn">` + html.EscapeString(status) + `</span>`
	}
}

func buildCuirassSearchURLServer(payload unavailableModePayload, item unavailablePackageItem) string {
	basePkg := item.Name
	if atIdx := strings.IndexByte(basePkg, '@'); atIdx > 0 {
		basePkg = basePkg[:atIdx]
	}
	isNg := item.Channel == "nonguix"
	serverURL := payload.ServerURL
	if serverURL == "" {
		serverURL = "https://ci.guix.gnu.org"
	}
	spec := payload.Spec
	if spec == "" {
		spec = "master"
	}
	if isNg {
		serverURL = payload.NonguixURL
		if serverURL == "" {
			serverURL = "https://cuirass.nonguix.org"
		}
		spec = "nonguix"
	}
	query := fmt.Sprintf("spec:%s system:%s %s", spec, payload.System, basePkg)
	return serverURL + "/search?query=" + url.QueryEscape(query)
}

func serverHost(rawURL string) string {
	if rawURL == "" {
		return "ci.guix.gnu.org"
	}
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	trimmed := strings.TrimPrefix(strings.TrimPrefix(rawURL, "https://"), "http://")
	if idx := strings.IndexByte(trimmed, '/'); idx != -1 {
		trimmed = trimmed[:idx]
	}
	if trimmed != "" {
		return trimmed
	}
	return "ci.guix.gnu.org"
}

func evaluationHref(serverURL string, evalID int64, sys string) string {
	return evaluationHrefWithStatus(serverURL, evalID, sys, "")
}

func evaluationHrefWithStatus(serverURL string, evalID int64, sys string, status string) string {
	host := serverHost(serverURL)
	var params []string
	if sys != "" && sys != "x86_64-linux" {
		params = append(params, "system="+url.QueryEscape(sys))
	}
	if status != "" {
		params = append(params, "status="+url.QueryEscape(status))
	}
	suffix := ""
	if len(params) > 0 {
		suffix = "?" + strings.Join(params, "&")
	}
	return fmt.Sprintf("/substitutes/%s/evaluations/%d%s", url.PathEscape(host), evalID, suffix)
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

			pullCell := renderPullCell(item.PullEvalID, item.PullReady, item.PullPendingJobs, item.PullFailedJobs, escServerURL, ov.System)

			evalPageURL := html.EscapeString(evaluationHref(escServerURL, item.EvaluationID, ov.System))
			readyURL := html.EscapeString(evaluationHrefWithStatus(escServerURL, item.EvaluationID, ov.System, "succeeded"))
			masterBadge := fmt.Sprintf(`<a href="%s" class="badge badge-ok" title="All non-stuck builds completed in evaluation #%d (click to inspect available substitutes)">Ready</a>`,
				readyURL, item.EvaluationID)
			if item.PendingJobs > 0 {
				pendingURL := html.EscapeString(evaluationHrefWithStatus(escServerURL, item.EvaluationID, ov.System, "pending"))
				masterBadge = fmt.Sprintf(`<a href="%s" class="badge badge-warn" title="%d jobs pending in evaluation #%d (click to inspect)">%d Pending</a>`,
					pendingURL, item.PendingJobs, item.EvaluationID, item.PendingJobs)
			}
			var masterFailedBadge string
			if item.FailedJobs > 0 {
				failedURL := html.EscapeString(evaluationHrefWithStatus(escServerURL, item.EvaluationID, ov.System, "failed"))
				masterFailedBadge = fmt.Sprintf(`<a href="%s" class="badge badge-danger" title="%d jobs failed or had a failed dependency in evaluation #%d (click to inspect)">%d Failed</a>`,
					failedURL, item.FailedJobs, item.EvaluationID, item.FailedJobs)
			}
			masterCell := fmt.Sprintf(`<div style="display:inline-flex;align-items:center;gap:0.4rem;flex-wrap:wrap;"><a href="%s" class="mono"><strong>#%d</strong></a>%s%s</div>`,
				evalPageURL, item.EvaluationID, masterBadge, masterFailedBadge)

			nonguixCell := renderChannelCell("nonguix", item, ov.System, isLatestNg)

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

func renderPullCell(pullEvalID int64, pullReady bool, pullPendingJobs int, pullFailedJobs int, serverURL string, sys string) string {
	if !cuirass.SupportsGuixJobset(sys) {
		return fmt.Sprintf(`<span class="badge badge-muted" title="ci.guix.gnu.org does not build the guix jobset for %s">Not built</span>`,
			html.EscapeString(sys))
	}
	if pullEvalID <= 0 {
		if pullReady {
			return `<span class="badge badge-ok">Ready</span>`
		}
		return `<span class="badge badge-warn">Pending</span>`
	}

	pullEvalURL := html.EscapeString(evaluationHref(serverURL, pullEvalID, sys))
	var badges strings.Builder
	if pullReady {
		readyURL := html.EscapeString(evaluationHrefWithStatus(serverURL, pullEvalID, sys, "succeeded"))
		fmt.Fprintf(&badges, `<a href="%s" class="badge badge-ok" title="guix pull substitute ready in evaluation #%d (click to inspect)">Ready</a>`,
			readyURL, pullEvalID)
	} else if pullPendingJobs == 0 && pullFailedJobs > 0 {
		failedURL := html.EscapeString(evaluationHrefWithStatus(serverURL, pullEvalID, sys, "failed"))
		fmt.Fprintf(&badges, `<a href="%s" class="badge badge-danger" title="%d guix pull jobs failed in evaluation #%d (click to inspect)">%d Failed</a>`,
			failedURL, pullFailedJobs, pullEvalID, pullFailedJobs)
	} else {
		pendingURL := html.EscapeString(evaluationHrefWithStatus(serverURL, pullEvalID, sys, "pending"))
		pendingLabel := "Pending"
		if pullPendingJobs > 0 {
			pendingLabel = fmt.Sprintf("%d Pending", pullPendingJobs)
		}
		fmt.Fprintf(&badges, `<a href="%s" class="badge badge-warn" title="guix pull substitute pending in evaluation #%d (click to inspect)">%s</a>`,
			pendingURL, pullEvalID, pendingLabel)
		if pullFailedJobs > 0 {
			failedURL := html.EscapeString(evaluationHrefWithStatus(serverURL, pullEvalID, sys, "failed"))
			fmt.Fprintf(&badges, `<a href="%s" class="badge badge-danger" title="%d guix pull jobs failed in evaluation #%d (click to inspect)">%d Failed</a>`,
				failedURL, pullFailedJobs, pullEvalID, pullFailedJobs)
		}
	}

	return fmt.Sprintf(`<div style="display:inline-flex;align-items:center;gap:0.4rem;flex-wrap:wrap;"><a href="%s" class="mono"><strong>#%d</strong></a>%s</div>`,
		pullEvalURL, pullEvalID, badges.String())
}

func renderChannelCell(channel string, item evaluationSummaryItem, sys string, _ bool) string {
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
	ngEvalPageURL := html.EscapeString(evaluationHref(chSum.ServerURL, chSum.EvalID, sys))
	var chFailedBadge string
	if chSum.FailedJobs > 0 {
		ngFailedURL := html.EscapeString(evaluationHrefWithStatus(chSum.ServerURL, chSum.EvalID, sys, "failed"))
		chFailedBadge = fmt.Sprintf(`<a href="%s" class="badge badge-danger" title="%d %s jobs failed in evaluation #%d (click to inspect)">%d Failed</a>`,
			ngFailedURL, chSum.FailedJobs, escChannel, chSum.EvalID, chSum.FailedJobs)
	}
	if chSum.Ready {
		ngReadyURL := html.EscapeString(evaluationHrefWithStatus(chSum.ServerURL, chSum.EvalID, sys, "succeeded"))
		return fmt.Sprintf(`<div style="display:inline-flex;align-items:center;gap:0.4rem;flex-wrap:wrap;"><a href="%s" class="mono" title="%s commit %s"><strong>#%d</strong></a><a href="%s" class="badge badge-ok" title="All %s builds completed in evaluation #%d (click to inspect available substitutes)">Ready</a>%s</div>`,
			ngEvalPageURL, escChannel, shortChCommit, chSum.EvalID, ngReadyURL, escChannel, chSum.EvalID, chFailedBadge)
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

	ngPendingURL := html.EscapeString(evaluationHrefWithStatus(chSum.ServerURL, chSum.EvalID, sys, "pending"))
	badgeLabel := "Pending"
	if chSum.PendingJobs > 0 {
		badgeLabel = fmt.Sprintf("%d Pending", chSum.PendingJobs)
	}
	if badgeTitleAttr == "" && chSum.PendingJobs > 0 {
		badgeTitleAttr = fmt.Sprintf(` title="%d %s jobs pending in evaluation #%d (click to inspect)"`,
			chSum.PendingJobs, escChannel, chSum.EvalID)
	}
	chBadge := fmt.Sprintf(`<a href="%s" class="badge badge-warn"%s>%s</a>`, ngPendingURL, badgeTitleAttr, badgeLabel)

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

	return fmt.Sprintf(`<div style="display:inline-flex;align-items:center;gap:0.4rem;flex-wrap:wrap;"><a href="%s" class="mono" title="%s commit %s"><strong>#%d</strong></a>%s%s%s</div>`,
		ngEvalPageURL, escChannel, shortChCommit, chSum.EvalID, chBadge, chFailedBadge, detailsHTML)
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
