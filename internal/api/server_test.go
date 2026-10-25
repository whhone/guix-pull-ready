package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"guix-evaluated/internal/cuirass"
	"guix-evaluated/internal/resolver"
	"guix-evaluated/internal/store"
)

func setupTestServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(":memory:", "https://ci.guix.gnu.org", []string{"x86_64-linux"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	now := time.Date(2026, 10, 17, 14, 0, 0, 0, time.UTC)
	st.UpdateEvaluations([]store.EvaluationRecord{
		{ID: 700, Commit: "aaaa1111bbbb2222cccc3333dddd4444eeee5555", Timestamp: now},
		{ID: 699, Commit: "ffff6666eeee7777dddd8888cccc9999bbbb0000", Timestamp: now.Add(-time.Hour)},
	}, 40)

	// Eval 700 has 1 pending build (libreoffice), but emacs & git succeeded
	st.SetPullStatus("aaaa1111bbbb2222cccc3333dddd4444eeee5555", "x86_64-linux", 1700, true)
	st.UpsertSystemEval(700, "aaaa1111bbbb2222cccc3333dddd4444eeee5555", "x86_64-linux", []cuirass.Job{
		{BuildID: 1, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
		{BuildID: 2, Name: "git.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "2.46.0"},
		{BuildID: 3, Name: "libreoffice.x86_64-linux", Status: cuirass.StatusScheduled, Version: "24.8"},
	})

	// Eval 699 has 0 pending builds
	st.SetPullStatus("ffff6666eeee7777dddd8888cccc9999bbbb0000", "x86_64-linux", 1699, true)
	st.UpsertSystemEval(699, "ffff6666eeee7777dddd8888cccc9999bbbb0000", "x86_64-linux", []cuirass.Job{
		{BuildID: 10, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
		{BuildID: 11, Name: "git.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "2.46.0"},
		{BuildID: 12, Name: "libreoffice.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "24.8"},
	})

	_ = st.Save()
	return NewServer(st, nil, nil)
}

func TestGetCommitWholeEvalAndPackageMode(t *testing.T) {
	srv := setupTestServer(t)

	// 1. Whole-evaluation mode (default max_pending=0) -> Eval 699
	req := httptest.NewRequest(http.MethodGet, "/commit?system=x86_64-linux", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "ffff6666eeee7777dddd8888cccc9999bbbb0000" {
		t.Fatalf("got commit %q, want ffff6666...", got)
	}

	// 2. Package mode via GET ?pkgs=emacs,git -> Eval 700
	req = httptest.NewRequest(http.MethodGet, "/commit?pkgs=emacs,git", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "aaaa1111bbbb2222cccc3333dddd4444eeee5555" {
		t.Fatalf("got commit %q, want aaaa1111...", got)
	}
}

func TestPostPackageManifestAndChannelsSCM(t *testing.T) {
	srv := setupTestServer(t)

	// 1. POST newline-separated package list to /channels.scm -> returns channels.scm snippet
	body := strings.NewReader("emacs\ngit\nmy-custom-pkg\n")
	req := httptest.NewRequest(http.MethodPost, "/channels.scm", body)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `(commit "aaaa1111bbbb2222cccc3333dddd4444eeee5555")`) {
		t.Fatalf("channels.scm missing expected commit:\n%s", out)
	}
	if !strings.Contains(out, "Skipped local:       my-custom-pkg") {
		t.Fatalf("channels.scm missing skipped local comment:\n%s", out)
	}
	if !strings.Contains(out, "make-channel-introduction") {
		t.Fatalf("channels.scm missing channel introduction:\n%s", out)
	}

	// 2. GET /channels.scm?pkgs=emacs,git
	req = httptest.NewRequest(http.MethodGet, "/channels.scm?pkgs=emacs,git", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	out = rec.Body.String()
	if !strings.Contains(out, `(commit "aaaa1111bbbb2222cccc3333dddd4444eeee5555")`) {
		t.Fatalf("channels.scm missing expected commit:\n%s", out)
	}
}

func TestStatusJSONAndHealthz(t *testing.T) {
	srv := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/status?system=x86_64-linux", nil)
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var res resolver.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if res.EvaluationID != 699 || !res.PullReady {
		t.Fatalf("unexpected status result: %+v", res)
	}

	// Evaluations endpoint
	req = httptest.NewRequest(http.MethodGet, "/evaluations?system=x86_64-linux", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var evalData map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &evalData); err != nil {
		t.Fatalf("evaluations json.Unmarshal: %v", err)
	}
	if evalData["system"] != "x86_64-linux" {
		t.Fatalf("expected system x86_64-linux, got %v", evalData["system"])
	}

	// Healthz
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", rec.Code)
	}
}

func TestDashboardHTMLAndPlainIndex(t *testing.T) {
	srv := setupTestServer(t)

	// 1. Browser request (HTML Overview page)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Guix Pull with All Prebuilts Ready",
		"About This Service",
		"Usage",
		"Recent Master Evaluations",
		"guix commit",
		"Date (Local)",
		`class="local-time" data-utc="`,
		"#700",
		"#699",
		`class="row-latest-ready"`,
		`latest-guix-badge" style="margin-left:0.35rem;">Latest Ready</span>`,
		`<pre class="mono">guix pull -C &lt;(curl -fsSL "http://example.com/channels.scm")</pre>`,
		`data-copy="guix pull -C &lt;(curl -fsSL &quot;http://example.com/channels.scm&quot;)" onclick="copyFromBtn(this)"`,
		"Track automatically via ~/.config/guix/channels.scm:",
		"(downloaded-channels",
		"color-scheme: light dark;",
		"@media (prefers-color-scheme: dark)",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("HTML overview missing %q", want)
		}
	}

	// Must NOT contain removed top commit block, status link, interactive resolver, poll badges, or arch switcher
	for _, unwanted := range []string{
		"Latest Commit with All Prebuilts Ready on Substitutes",
		`<a href="/status">Status</a>`,
		"Interactive Commit &amp; Package Resolver",
		"Latest Master Evaluation",
		"Evaluations:",
		"Poller Idle",
		"Syncing Cuirass",
		"arch-bar",
		"aarch64-linux",
		`/healthz" class="btn mono"`,
		`/evaluations?system=`,
	} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("HTML overview should not contain %q", unwanted)
		}
	}

	// 2. curl request (Plain-Text Summary)
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("User-Agent", "curl/8.5.0")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	plain := rec.Body.String()
	if !strings.Contains(plain, "ffff6666eeee7777dddd8888cccc9999bbbb0000") {
		t.Fatalf("plain index missing ready commit:\n%s", plain)
	}
	if !strings.Contains(plain, "downloaded-channels") {
		t.Fatalf("plain index missing downloaded-channels:\n%s", plain)
	}
}

func TestStatusEndpointAlwaysReturnsJSON(t *testing.T) {
	srv := setupTestServer(t)

	// 1. Browser request to /status (Accept: text/html) -> JSON API response
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var res resolver.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if res.EvaluationID != 699 {
		t.Fatalf("got eval %d, want 699", res.EvaluationID)
	}

	// 2. Package query to /status?pkgs=emacs,git -> JSON API response
	req = httptest.NewRequest(http.MethodGet, "/status?pkgs=emacs,git", nil)
	req.Header.Set("Accept", "text/html")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /status?pkgs=... status = %d, want 200", rec.Code)
	}
	var pkgRes resolver.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &pkgRes); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if pkgRes.EvaluationID != 700 {
		t.Fatalf("got eval %d, want 700", pkgRes.EvaluationID)
	}
}
func TestRequirePullAlwaysEnforced(t *testing.T) {
	st, err := store.Open(":memory:", "https://ci.guix.gnu.org", []string{"x86_64-linux"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	st.UpdateEvaluations([]store.EvaluationRecord{
		{ID: 801, Commit: "8888111122223333444455556666777788889999", Timestamp: now},
		{ID: 800, Commit: "7777111122223333444455556666777788889999", Timestamp: now.Add(-time.Hour)},
	}, 40)

	// Eval 801 has completed jobs, but pull is NOT ready (e.g. guix job pending/failed)
	st.SetPullStatus("8888111122223333444455556666777788889999", "x86_64-linux", 1801, false)
	st.UpsertSystemEval(801, "8888111122223333444455556666777788889999", "x86_64-linux", []cuirass.Job{
		{BuildID: 101, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
	})

	// Eval 800 has completed jobs and pull IS ready
	st.SetPullStatus("7777111122223333444455556666777788889999", "x86_64-linux", 1800, true)
	st.UpsertSystemEval(800, "7777111122223333444455556666777788889999", "x86_64-linux", []cuirass.Job{
		{BuildID: 100, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
	})

	_ = st.Save()
	srv := NewServer(st, nil, nil)

	// 1. Whole-eval mode: Eval 801 is skipped because pull is not ready, and Eval 800 is selected
	req := httptest.NewRequest(http.MethodGet, "/commit", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "7777111122223333444455556666777788889999" {
		t.Fatalf("got commit %q, want eval 800 commit (77771111...)", got)
	}

	// 2. Package mode: Eval 801 is also skipped because pull is not ready, and Eval 800 is selected
	req = httptest.NewRequest(http.MethodGet, "/commit?pkgs=emacs", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "7777111122223333444455556666777788889999" {
		t.Fatalf("got commit %q, want eval 800 commit (77771111...)", got)
	}
}

func TestAdditionalChannelsNonguixAPI(t *testing.T) {
	st, err := store.Open(":memory:", "https://ci.guix.gnu.org", []string{"x86_64-linux"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.SetChannelServerURL("nonguix", "https://cuirass.nonguix.org")

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	st.UpdateEvaluations([]store.EvaluationRecord{
		{ID: 700, Commit: "aaaa1111bbbb2222cccc3333dddd4444eeee5555", Timestamp: now},
		{ID: 699, Commit: "ffff6666eeee7777dddd8888cccc9999bbbb0000", Timestamp: now.Add(-time.Hour)},
	}, 40)

	// Eval 700 has 1 pending build (libreoffice)
	st.SetPullStatus("aaaa1111bbbb2222cccc3333dddd4444eeee5555", "x86_64-linux", 1700, true)
	st.UpsertSystemEval(700, "aaaa1111bbbb2222cccc3333dddd4444eeee5555", "x86_64-linux", []cuirass.Job{
		{BuildID: 1, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
		{BuildID: 2, Name: "libreoffice.x86_64-linux", Status: cuirass.StatusScheduled, Version: "24.8"},
	})

	// Eval 699 has 0 pending builds
	st.SetPullStatus("ffff6666eeee7777dddd8888cccc9999bbbb0000", "x86_64-linux", 1699, true)
	st.UpsertSystemEval(699, "ffff6666eeee7777dddd8888cccc9999bbbb0000", "x86_64-linux", []cuirass.Job{
		{BuildID: 10, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
		{BuildID: 11, Name: "libreoffice.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "24.8"},
	})

	st.UpdateChannelEvaluations("nonguix", []store.EvaluationRecord{
		{
			ID:      55,
			Channel: "nonguix",
			Commit:  "ng55111122223333444455556666777788889999",
			Checkouts: map[string]string{
				"nonguix": "ng55111122223333444455556666777788889999",
				"guix":    "aaaa1111bbbb2222cccc3333dddd4444eeee5555",
			},
			Timestamp: now,
		},
		{
			ID:      54,
			Channel: "nonguix",
			Commit:  "ng54111122223333444455556666777788889999",
			Checkouts: map[string]string{
				"nonguix": "ng54111122223333444455556666777788889999",
				"guix":    "ffff6666eeee7777dddd8888cccc9999bbbb0000",
			},
			Timestamp: now.Add(-time.Hour),
		},
	}, 40)

	st.UpsertChannelSystemEval("nonguix", 55, "ng55111122223333444455556666777788889999", "x86_64-linux", []cuirass.Job{
		{BuildID: 901, Name: "linux.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "6.12.1"},
	})
	st.UpsertChannelSystemEval("nonguix", 54, "ng54111122223333444455556666777788889999", "x86_64-linux", []cuirass.Job{
		{BuildID: 902, Name: "linux.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "6.12.1"},
	})

	_ = st.Save()
	srv := NewServer(st, nil, nil)

	// 1. GET /commit?additional_channels=nonguix (whole-eval mode -> picks Guix 699 + Nonguix 54)
	req := httptest.NewRequest(http.MethodGet, "/commit?additional_channels=nonguix", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "ffff6666eeee7777dddd8888cccc9999bbbb0000" {
		t.Fatalf("got commit %q, want ffff6666...", got)
	}

	// 2. GET /channels.scm?additional_channels=nonguix&pkgs=emacs,linux (package mode -> picks Guix 700 + Nonguix 55)
	req = httptest.NewRequest(http.MethodGet, "/channels.scm?additional_channels=nonguix&pkgs=emacs,linux", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	scm := rec.Body.String()
	if !strings.Contains(scm, `(name 'nonguix)`) ||
		!strings.Contains(scm, `(commit "ng55111122223333444455556666777788889999")`) ||
		!strings.Contains(scm, `(commit "aaaa1111bbbb2222cccc3333dddd4444eeee5555")`) {
		t.Fatalf("channels.scm missing expected nonguix + guix commits:\n%s", scm)
	}

	// 3. GET /status?additional_channels=nonguix -> returns JSON with additional_channels
	req = httptest.NewRequest(http.MethodGet, "/status?additional_channels=nonguix", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var statusRes resolver.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &statusRes); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(statusRes.AdditionalChannels) != 1 || statusRes.AdditionalChannels[0].Channel != "nonguix" || statusRes.AdditionalChannels[0].EvaluationID != 54 {
		t.Fatalf("unexpected additional_channels in /status response: %+v", statusRes.AdditionalChannels)
	}

	// 4. Browser GET / and GET /?additional_channels=nonguix -> toggles nonguix state and column
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/html")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `<th class="col-nonguix" style="display:none;"><a href="https://cuirass.nonguix.org/jobset/nonguix" target="_blank" rel="noopener">nonguix/nonguix</a></th>`) {
		t.Fatalf("home HTML without additional_channels should hide nonguix column by default")
	}

	req = httptest.NewRequest(http.MethodGet, "/?additional_channels=nonguix", nil)
	req.Header.Set("Accept", "text/html")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	homeHTML := rec.Body.String()
	if !strings.Contains(homeHTML, `id="main-nonguix-toggle" checked`) ||
		!strings.Contains(homeHTML, `id="main-cli-guix" class="examples-list" style="display:none;"`) ||
		!strings.Contains(homeHTML, `id="main-cli-nonguix" class="examples-list"`) ||
		!strings.Contains(homeHTML, `https://gitlab.com/nonguix/nonguix/#substitutes-for-nonguix`) ||
		!strings.Contains(homeHTML, `<th><a href="https://ci.guix.gnu.org/jobset/guix" target="_blank" rel="noopener">guix/guix</a></th>`) ||
		!strings.Contains(homeHTML, `<th><a href="https://ci.guix.gnu.org/jobset/master" target="_blank" rel="noopener">guix/master</a></th>`) ||
		!strings.Contains(homeHTML, `<th class="col-nonguix"><a href="https://cuirass.nonguix.org/jobset/nonguix" target="_blank" rel="noopener">nonguix/nonguix</a></th>`) ||
		!strings.Contains(homeHTML, `https://cuirass.nonguix.org/eval/55`) ||
		!strings.Contains(homeHTML, `<strong>#55</strong>`) ||
		!strings.Contains(homeHTML, `class="row-latest-ready"`) ||
		!strings.Contains(homeHTML, `latest-nonguix-badge" style="margin-left:0.35rem;">Latest Ready</span>`) {
		t.Fatalf("home HTML with ?additional_channels=nonguix did not pre-select nonguix state, table column, latest ready row, or substitute setup link")
	}

	// 5. GET /commit?additional_channels=invalid -> 400 Bad Request
	req = httptest.NewRequest(http.MethodGet, "/commit?additional_channels=invalid", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
