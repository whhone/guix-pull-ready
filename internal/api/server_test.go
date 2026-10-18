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
	req := httptest.NewRequest(http.MethodGet, "/v1/commit?system=x86_64-linux", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "ffff6666eeee7777dddd8888cccc9999bbbb0000" {
		t.Fatalf("got commit %q, want ffff6666...", got)
	}

	// 2. Package mode via GET ?pkgs=emacs,git -> Eval 700
	req = httptest.NewRequest(http.MethodGet, "/v1/commit?pkgs=emacs,git", nil)
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

	// POST newline-separated package list to /v1/channels.scm
	body := strings.NewReader("emacs\ngit\nmy-custom-pkg\n")
	req := httptest.NewRequest(http.MethodPost, "/v1/channels.scm", body)
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
}

func TestStatusJSONAndHealthz(t *testing.T) {
	srv := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/status?system=x86_64-linux", nil)
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
		"guix-sync-commit",
		"Latest Commit with All Prebuilts Ready on Substitutes",
		"Usages",
		`<a href="/status">Status</a>`,
		`<pre class="mono">guix pull --commit=ffff6666eeee7777dddd8888cccc9999bbbb0000</pre>`,
		`data-copy="guix pull --commit=ffff6666eeee7777dddd8888cccc9999bbbb0000" onclick="copyFromBtn(this)"`,
		`data-copy="ffff6666eeee7777dddd8888cccc9999bbbb0000" onclick="copyFromBtn(this)">Copy Commit</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("HTML overview missing %q", want)
		}
	}

	// Must NOT contain detailed status/resolver/evaluations table, poll badges, or arch switcher on the overview page
	for _, unwanted := range []string{
		"Interactive Commit &amp; Package Resolver",
		"Recent Master Evaluations",
		"Latest Master Evaluation",
		"Evaluations:",
		"Poller Idle",
		"Syncing Cuirass",
		"arch-bar",
		"aarch64-linux",
		`/healthz" class="btn mono"`,
		`/v1/evaluations?system=`,
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
}

func TestStatusPageHTMLAndContentNegotiation(t *testing.T) {
	srv := setupTestServer(t)

	// 1. Browser request to /status (Accept: text/html) -> HTML Status page
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	htmlBody := rec.Body.String()
	for _, want := range []string{
		"guix-sync-commit Status",
		`class="active">Status</a>`,
		"Latest Ready Commit",
		"Latest Master Evaluation",
		"Interactive Commit &amp; Package Resolver",
		"Recent Master Evaluations",
		"Date (Local)",
		`class="local-time" data-utc="`,
		"#700",
		"#699",
		"libreoffice",
	} {
		if !strings.Contains(htmlBody, want) {
			t.Fatalf("HTML status page missing %q", want)
		}
	}

	// Clean header check: no raw /healthz, /v1/evaluations, "Evaluations:", "Poller Idle", or arch switcher
	for _, unwanted := range []string{
		"Evaluations:",
		"Poller Idle",
		"Syncing Cuirass",
		"arch-bar",
		"aarch64-linux",
		`/healthz" class="btn mono"`,
		`/v1/evaluations?system=`,
	} {
		if strings.Contains(htmlBody, unwanted) {
			t.Fatalf("HTML status page should not contain %q", unwanted)
		}
	}

	// 2. curl request to /status -> JSON API response
	req = httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("User-Agent", "curl/8.5.0")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("curl /status status = %d, want 200", rec.Code)
	}
	var res resolver.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if res.EvaluationID != 699 {
		t.Fatalf("got eval %d, want 699", res.EvaluationID)
	}

	// 3. Package query to /status?pkgs=emacs,git -> JSON API response
	req = httptest.NewRequest(http.MethodGet, "/status?pkgs=emacs,git", nil)
	req.Header.Set("Accept", "text/html") // Even with text/html, package query should return JSON
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

