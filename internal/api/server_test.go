package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"guix-pull-ready/internal/cuirass"
	"guix-pull-ready/internal/store"
)

func setupTestServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(":memory:", "https://ci.guix.gnu.org", cuirass.DefaultSystems)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	now := time.Date(2026, 10, 17, 14, 0, 0, 0, time.UTC)
	st.UpdateChannelEvaluations("guix", []store.EvaluationRecord{
		{ID: 700, Commit: "aaaa1111bbbb2222cccc3333dddd4444eeee5555", Timestamp: now},
		{ID: 699, Commit: "ffff6666eeee7777dddd8888cccc9999bbbb0000", Timestamp: now.Add(-time.Hour)},
	}, 40)

	// x86_64-linux: Eval 700 has 1 pending build (libreoffice), but emacs & git succeeded
	st.SetPullStatus("aaaa1111bbbb2222cccc3333dddd4444eeee5555", "x86_64-linux", 1700, true)
	st.UpsertChannelSystemEval("guix", 700, "aaaa1111bbbb2222cccc3333dddd4444eeee5555", "x86_64-linux", []cuirass.Job{
		{BuildID: 1, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
		{BuildID: 2, Name: "git.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "2.46.0"},
		{BuildID: 3, Name: "libreoffice.x86_64-linux", Status: cuirass.StatusScheduled, Version: "24.8"},
	})

	// x86_64-linux: Eval 699 has 0 pending builds
	st.SetPullStatus("ffff6666eeee7777dddd8888cccc9999bbbb0000", "x86_64-linux", 1699, true)
	st.UpsertChannelSystemEval("guix", 699, "ffff6666eeee7777dddd8888cccc9999bbbb0000", "x86_64-linux", []cuirass.Job{
		{BuildID: 10, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
		{BuildID: 11, Name: "git.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "2.46.0"},
		{BuildID: 12, Name: "libreoffice.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "24.8"},
	})

	// aarch64-linux: Eval 700 has 0 pending builds (already complete on aarch64; no guix/guix jobset exists on ci.guix.gnu.org for aarch64)
	st.UpsertChannelSystemEval("guix", 700, "aaaa1111bbbb2222cccc3333dddd4444eeee5555", "aarch64-linux", []cuirass.Job{
		{BuildID: 21, Name: "emacs.aarch64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
		{BuildID: 22, Name: "git.aarch64-linux", Status: cuirass.StatusSucceeded, Version: "2.46.0"},
	})

	// aarch64-linux: Eval 699 has 1 pending build
	st.UpsertChannelSystemEval("guix", 699, "ffff6666eeee7777dddd8888cccc9999bbbb0000", "aarch64-linux", []cuirass.Job{
		{BuildID: 30, Name: "emacs.aarch64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
		{BuildID: 31, Name: "git.aarch64-linux", Status: cuirass.StatusScheduled, Version: "2.46.0"},
	})

	// powerpc64le-linux: Eval 700 has 0 pending builds
	st.UpsertChannelSystemEval("guix", 700, "aaaa1111bbbb2222cccc3333dddd4444eeee5555", "powerpc64le-linux", []cuirass.Job{
		{BuildID: 41, Name: "emacs.powerpc64le-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
	})

	// riscv64-linux: Eval 699 has 0 pending builds
	st.UpsertChannelSystemEval("guix", 699, "ffff6666eeee7777dddd8888cccc9999bbbb0000", "riscv64-linux", []cuirass.Job{
		{BuildID: 51, Name: "emacs.riscv64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
	})

	_ = st.Save()
	return NewServer(st, nil, nil)
}

func TestChannelsSCMWholeEvalAndPackageMode(t *testing.T) {
	srv := setupTestServer(t)

	// 1. Whole-evaluation mode on x86_64-linux (default max_pending=0) -> Eval 699
	req := httptest.NewRequest(http.MethodGet, "/channels.scm?system=x86_64-linux", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `(commit "ffff6666eeee7777dddd8888cccc9999bbbb0000")`) {
		t.Fatalf("got body %q, want ffff6666...", rec.Body.String())
	}

	// 2. Whole-evaluation mode on aarch64-linux -> Eval 700
	req = httptest.NewRequest(http.MethodGet, "/channels.scm?system=aarch64-linux", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), ";; System:              aarch64-linux") ||
		!strings.Contains(rec.Body.String(), ";; guix pull status:    Not built by ci.guix.gnu.org for aarch64-linux (guix/master substitutes ready)") ||
		!strings.Contains(rec.Body.String(), `(commit "aaaa1111bbbb2222cccc3333dddd4444eeee5555")`) {
		t.Fatalf("got body %q, want aarch64-linux and aaaa1111...", rec.Body.String())
	}

	// 3. Package mode via GET ?pkgs=emacs,git -> Eval 700
	req = httptest.NewRequest(http.MethodGet, "/channels.scm?pkgs=emacs,git", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `(commit "aaaa1111bbbb2222cccc3333dddd4444eeee5555")`) {
		t.Fatalf("got body %q, want aaaa1111...", rec.Body.String())
	}

	// 4. Dropped /commit endpoint returns 404
	req = httptest.NewRequest(http.MethodGet, "/commit", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /commit status = %d, want 404", rec.Code)
	}
}

func TestChannelsSCM(t *testing.T) {
	srv := setupTestServer(t)

	// 1. GET /channels.scm?pkgs=emacs,git,my-custom-pkg -> returns channels.scm snippet
	req := httptest.NewRequest(http.MethodGet, "/channels.scm?pkgs=emacs,git,my-custom-pkg", nil)
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

	// 2. POST /channels.scm -> rejected with 400 Bad Request
	req = httptest.NewRequest(http.MethodPost, "/channels.scm", strings.NewReader("emacs\ngit\n"))
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST status = %d, want 400", rec.Code)
	}
}

func TestHealthzEndpointNotFound(t *testing.T) {
	srv := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("healthz status = %d, want 404", rec.Code)
	}
}

func TestDashboardHTML(t *testing.T) {
	srv := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Guix Pull Ready",
		`id="arch-select"`,
		`<option value="x86_64-linux" selected>x86_64-linux</option>`,
		`<option value="aarch64-linux">aarch64-linux</option>`,
		`<option value="powerpc64le-linux">powerpc64le-linux</option>`,
		`<option value="riscv64-linux">riscv64-linux</option>`,
		"About This Service",
		"Usage",
		"Recent Master Evaluations",
		"guix commit",
		"Committed",
		`class="local-time" data-utc="`,
		"#700",
		"#699",
		"#1700",
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

	// Must NOT contain removed top commit block, status link, interactive resolver, or poll badges
	for _, unwanted := range []string{
		"Latest Commit with All Prebuilts Ready on Substitutes",
		`<a href="/status">Status</a>`,
		"Interactive Commit &amp; Package Resolver",
		"Latest Master Evaluation",
		"Evaluations:",
		"Poller Idle",
		"Syncing Cuirass",
		`/healthz" class="btn mono"`,
		`/evaluations?system=`,
	} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("HTML overview should not contain %q", unwanted)
		}
	}

	// Verify GET /?system=aarch64-linux selects aarch64-linux in the dropdown, shows aarch64 evaluations (#2700),
	// and updates the /channels.scm CLI commands with ?system=aarch64-linux
	req = httptest.NewRequest(http.MethodGet, "/?system=aarch64-linux", nil)
	req.Header.Set("Accept", "text/html")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /?system=aarch64-linux status = %d, want 200", rec.Code)
	}
	aarchBody := rec.Body.String()
	for _, want := range []string{
		`<a href="/?system=aarch64-linux">Guix Pull Ready</a>`,
		`<option value="x86_64-linux">x86_64-linux</option>`,
		`<option value="aarch64-linux" selected>aarch64-linux</option>`,
		`<span class="badge badge-muted" title="ci.guix.gnu.org does not build the guix jobset for aarch64-linux">Not built</span>`,
		`<pre class="mono">guix pull -C &lt;(curl -fsSL "http://example.com/channels.scm?system=aarch64-linux")</pre>`,
		`data-copy="guix pull -C &lt;(curl -fsSL &quot;http://example.com/channels.scm?system=aarch64-linux&quot;)" onclick="copyFromBtn(this)"`,
		`<pre class="mono">(downloaded-channels "http://example.com/channels.scm?system=aarch64-linux")</pre>`,
		`<pre class="mono">guix pull -C &lt;(curl -fsSL "http://example.com/channels.scm?system=aarch64-linux&amp;additional_channels=nonguix")</pre>`,
	} {
		if !strings.Contains(aarchBody, want) {
			t.Fatalf("GET /?system=aarch64-linux missing %q", want)
		}
	}

	// Verify GET /?system=powerpc64le-linux and GET /?system=riscv64-linux show Not built for guix/guix and nonguix/nonguix
	req = httptest.NewRequest(http.MethodGet, "/?system=powerpc64le-linux", nil)
	req.Header.Set("Accept", "text/html")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /?system=powerpc64le-linux status = %d, want 200", rec.Code)
	}
	ppcBody := rec.Body.String()
	for _, want := range []string{
		`<option value="powerpc64le-linux" selected>powerpc64le-linux</option>`,
		`<span class="badge badge-muted" title="ci.guix.gnu.org does not build the guix jobset for powerpc64le-linux">Not built</span>`,
		`<span class="badge badge-muted" title="cuirass.nonguix.org does not build substitutes for powerpc64le-linux">Not built</span>`,
	} {
		if !strings.Contains(ppcBody, want) {
			t.Fatalf("GET /?system=powerpc64le-linux missing %q", want)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/?system=riscv64-linux", nil)
	req.Header.Set("Accept", "text/html")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /?system=riscv64-linux status = %d, want 200", rec.Code)
	}
	riscvBody := rec.Body.String()
	for _, want := range []string{
		`<option value="riscv64-linux" selected>riscv64-linux</option>`,
		`<span class="badge badge-muted" title="ci.guix.gnu.org does not build the guix jobset for riscv64-linux">Not built</span>`,
		`<span class="badge badge-muted" title="cuirass.nonguix.org does not build substitutes for riscv64-linux">Not built</span>`,
	} {
		if !strings.Contains(riscvBody, want) {
			t.Fatalf("GET /?system=riscv64-linux missing %q", want)
		}
	}
}

func TestDashboardDropdownConsistentWithConfiguredSystems(t *testing.T) {
	// Configure store with only x86_64-linux
	st, err := store.Open(":memory:", "https://ci.guix.gnu.org", []string{"x86_64-linux"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	now := time.Date(2026, 10, 17, 14, 0, 0, 0, time.UTC)
	st.UpdateChannelEvaluations("guix", []store.EvaluationRecord{
		{ID: 700, Commit: "aaaa1111bbbb2222cccc3333dddd4444eeee5555", Timestamp: now},
	}, 40)
	st.SetPullStatus("aaaa1111bbbb2222cccc3333dddd4444eeee5555", "x86_64-linux", 1700, true)
	st.UpsertChannelSystemEval("guix", 700, "aaaa1111bbbb2222cccc3333dddd4444eeee5555", "x86_64-linux", []cuirass.Job{
		{BuildID: 1, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
	})
	_ = st.Save()

	srv := NewServer(st, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<option value="x86_64-linux" selected>x86_64-linux</option>`) {
		t.Fatalf("expected x86_64-linux in dropdown")
	}
	for _, unwanted := range []string{
		`<option value="aarch64-linux"`,
		`<option value="powerpc64le-linux"`,
		`<option value="riscv64-linux"`,
	} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("dropdown contains unwanted system option %q when --systems only tracks x86_64-linux", unwanted)
		}
	}
}

func TestStatusEndpointNotFound(t *testing.T) {
	srv := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /status code = %d, want 404 (endpoint removed)", rec.Code)
	}
}
func TestRequirePullAlwaysEnforced(t *testing.T) {
	st, err := store.Open(":memory:", "https://ci.guix.gnu.org", []string{"x86_64-linux"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	st.UpdateChannelEvaluations("guix", []store.EvaluationRecord{
		{ID: 801, Commit: "8888111122223333444455556666777788889999", Timestamp: now},
		{ID: 800, Commit: "7777111122223333444455556666777788889999", Timestamp: now.Add(-time.Hour)},
	}, 40)

	// Eval 801 has completed jobs, but pull is NOT ready (e.g. guix job pending/failed)
	st.SetPullStatus("8888111122223333444455556666777788889999", "x86_64-linux", 1801, false)
	st.UpsertChannelSystemEval("guix", 801, "8888111122223333444455556666777788889999", "x86_64-linux", []cuirass.Job{
		{BuildID: 101, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
	})

	// Eval 800 has completed jobs and pull IS ready
	st.SetPullStatus("7777111122223333444455556666777788889999", "x86_64-linux", 1800, true)
	st.UpsertChannelSystemEval("guix", 800, "7777111122223333444455556666777788889999", "x86_64-linux", []cuirass.Job{
		{BuildID: 100, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
	})

	_ = st.Save()
	srv := NewServer(st, nil, nil)

	// 1. Whole-eval mode: Eval 801 is skipped because pull is not ready, and Eval 800 is selected
	req := httptest.NewRequest(http.MethodGet, "/channels.scm", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `(commit "7777111122223333444455556666777788889999")`) {
		t.Fatalf("got body %q, want eval 800 commit (77771111...)", rec.Body.String())
	}

	// 2. Package mode: Eval 801 is also skipped because pull is not ready, and Eval 800 is selected
	req = httptest.NewRequest(http.MethodGet, "/channels.scm?pkgs=emacs", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `(commit "7777111122223333444455556666777788889999")`) {
		t.Fatalf("got body %q, want eval 800 commit (77771111...)", rec.Body.String())
	}
}

func TestAdditionalChannelsNonguixAPI(t *testing.T) {
	st, err := store.Open(":memory:", "https://ci.guix.gnu.org", []string{"x86_64-linux"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.SetChannelServerURL("nonguix", "https://cuirass.nonguix.org")

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	st.UpdateChannelEvaluations("guix", []store.EvaluationRecord{
		{ID: 700, Commit: "aaaa1111bbbb2222cccc3333dddd4444eeee5555", Timestamp: now},
		{ID: 699, Commit: "ffff6666eeee7777dddd8888cccc9999bbbb0000", Timestamp: now.Add(-time.Hour)},
	}, 40)

	// Eval 700 has 1 pending build (libreoffice)
	st.SetPullStatus("aaaa1111bbbb2222cccc3333dddd4444eeee5555", "x86_64-linux", 1700, true)
	st.UpsertChannelSystemEval("guix", 700, "aaaa1111bbbb2222cccc3333dddd4444eeee5555", "x86_64-linux", []cuirass.Job{
		{BuildID: 1, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
		{BuildID: 2, Name: "libreoffice.x86_64-linux", Status: cuirass.StatusScheduled, Version: "24.8"},
	})

	// Eval 699 has 0 pending builds
	st.SetPullStatus("ffff6666eeee7777dddd8888cccc9999bbbb0000", "x86_64-linux", 1699, true)
	st.UpsertChannelSystemEval("guix", 699, "ffff6666eeee7777dddd8888cccc9999bbbb0000", "x86_64-linux", []cuirass.Job{
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
		{BuildID: 1005984, Name: "firefox.x86_64-linux", Status: cuirass.StatusScheduled, Version: "133.0"},
	})
	st.UpsertChannelSystemEval("nonguix", 54, "ng54111122223333444455556666777788889999", "x86_64-linux", []cuirass.Job{
		{BuildID: 902, Name: "linux.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "6.12.1"},
	})

	_ = st.Save()
	srv := NewServer(st, nil, nil)

	// 1. GET /channels.scm?additional_channels=nonguix (whole-eval mode -> picks Guix 699 + Nonguix 54)
	req := httptest.NewRequest(http.MethodGet, "/channels.scm?additional_channels=nonguix", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `(commit "ffff6666eeee7777dddd8888cccc9999bbbb0000")`) ||
		!strings.Contains(rec.Body.String(), `(commit "ng54111122223333444455556666777788889999")`) {
		t.Fatalf("got body %q, want ffff6666... and ng541111...", rec.Body.String())
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

	// 3. Browser GET / and GET /?additional_channels=nonguix -> toggles nonguix state and column
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
		!strings.Contains(homeHTML, `/substitutes/cuirass.nonguix.org/evaluations/55`) ||
		!strings.Contains(homeHTML, `<strong>#55</strong>`) ||
		!strings.Contains(homeHTML, `<a href="/substitutes/cuirass.nonguix.org/evaluations/55?status=pending" class="badge badge-warn" title="Pending jobs: firefox.x86_64-linux">1 Pending</a>`) ||
		!strings.Contains(homeHTML, `<a href="https://cuirass.nonguix.org/build/1005984/details" target="_blank" rel="noopener" class="mono" title="Pending build #1005984 (firefox.x86_64-linux)">firefox</a>`) ||
		!strings.Contains(homeHTML, `class="row-latest-ready"`) ||
		!strings.Contains(homeHTML, `latest-nonguix-badge" style="margin-left:0.35rem;">Latest Ready</span>`) {
		t.Fatalf("home HTML with ?additional_channels=nonguix did not pre-select nonguix state, table column, pending build link, latest ready row, or substitute setup link")
	}

	// 4. GET /channels.scm?additional_channels=invalid -> 400 Bad Request
	req = httptest.NewRequest(http.MethodGet, "/channels.scm?additional_channels=invalid", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestFaviconEndpoint(t *testing.T) {
	srv := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /favicon.ico status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Fatalf("GET /favicon.ico Content-Type = %q, want image/svg+xml", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=") {
		t.Fatalf("GET /favicon.ico Cache-Control = %q, want max-age header", cc)
	}
	if !strings.Contains(rec.Body.String(), "<svg") || !strings.Contains(rec.Body.String(), "</svg>") {
		t.Fatalf("GET /favicon.ico body does not contain valid SVG markup")
	}

	// HTML dashboard should link the favicon
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<link rel="icon" type="image/svg+xml" href="/favicon.ico">`) {
		t.Fatalf("GET / HTML missing favicon link")
	}
}

func TestDashboardUnavailablePackagesAndFailedBadges(t *testing.T) {
	st, err := store.Open(":memory:", "https://ci.guix.gnu.org", []string{"x86_64-linux"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.SetChannelServerURL("nonguix", "https://cuirass.nonguix.org")

	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	st.UpdateChannelEvaluations("guix", []store.EvaluationRecord{
		{ID: 700, Commit: "aaaa1111bbbb2222cccc3333dddd4444eeee5555", Timestamp: now},
		{ID: 699, Commit: "ffff6666eeee7777dddd8888cccc9999bbbb0000", Timestamp: now.Add(-time.Hour)},
	}, 40)

	// Eval 700 has 1 pending build, 1 failed build, and 1 package (omitted-pkg) that won't be in Eval 699
	st.SetPullStatus("aaaa1111bbbb2222cccc3333dddd4444eeee5555", "x86_64-linux", 1700, true)
	st.UpsertChannelSystemEval("guix", 700, "aaaa1111bbbb2222cccc3333dddd4444eeee5555", "x86_64-linux", []cuirass.Job{
		{BuildID: 1, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
		{BuildID: 2, Name: "libreoffice.x86_64-linux", Status: cuirass.StatusScheduled, Version: "24.8"},
		{BuildID: 3, Name: "ruby-mysql2.x86_64-linux", Status: cuirass.StatusFailed, Version: "0.5.5"},
		{BuildID: 4, Name: "omitted-pkg.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "1.0"},
	})

	// Eval 699 (Latest Ready) has 0 pending builds, 2 failed jobs (ruby-mysql2 failed, github-cli failed-dependency),
	// 1 multi-version package where one version failed (gcc-toolchain@13), and omitted-pkg is not built.
	st.SetPullStatus("ffff6666eeee7777dddd8888cccc9999bbbb0000", "x86_64-linux", 1699, true)
	st.UpsertChannelSystemEval("guix", 699, "ffff6666eeee7777dddd8888cccc9999bbbb0000", "x86_64-linux", []cuirass.Job{
		{BuildID: 10, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
		{BuildID: 15, Name: "libreoffice.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "24.8"},
		{BuildID: 11, Name: "ruby-mysql2.x86_64-linux", Status: cuirass.StatusFailed, Version: "0.5.5"},
		{BuildID: 12, Name: "github-cli.x86_64-linux", Status: cuirass.StatusFailedDependency, Version: "2.52.0"},
		{BuildID: 13, Name: "gcc-toolchain.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "14.2.0"},
		{BuildID: 14, Name: "gcc-toolchain.x86_64-linux", Status: cuirass.StatusFailed, Version: "13.3.0"},
	})

	st.UpdateChannelEvaluations("nonguix", []store.EvaluationRecord{
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
	st.UpsertChannelSystemEval("nonguix", 54, "ng54111122223333444455556666777788889999", "x86_64-linux", []cuirass.Job{
		{BuildID: 901, Name: "linux.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "6.12.1"},
		{BuildID: 902, Name: "nvidia-driver.x86_64-linux", Status: cuirass.StatusFailed, Version: "550.120"},
	})

	_ = st.Save()
	srv := NewServer(st, nil, nil)

	// 1. Default dashboard (Guix mode): no unavailable-panel on /, evaluation links point to /substitutes/<server>/evaluations/<id>
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, unwanted := range []string{
		`id="unavailable-panel"`,
		`Unavailable Packages in Latest Ready Commit`,
		`<script id="unavailable-pkgs-data" type="application/json">`,
	} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("GET / should not contain %q", unwanted)
		}
	}
	for _, want := range []string{
		`<a href="/substitutes/ci.guix.gnu.org/evaluations/699" class="mono"><strong>#699</strong></a>`,
		`<a href="/substitutes/ci.guix.gnu.org/evaluations/699?status=succeeded" class="badge badge-ok"`,
		`<a href="/substitutes/ci.guix.gnu.org/evaluations/700" class="mono"><strong>#700</strong></a>`,
		`<a href="/substitutes/ci.guix.gnu.org/evaluations/700?status=pending" class="badge badge-warn" title="1 jobs pending in evaluation #700 (click to inspect)">1 Pending</a>`,
		`<a href="/substitutes/ci.guix.gnu.org/evaluations/699?status=failed" class="badge badge-danger" title="3 jobs failed or had a failed dependency in evaluation #699 (click to inspect)">3 Failed</a>`,
		`<a href="/substitutes/ci.guix.gnu.org/evaluations/700?status=failed" class="badge badge-danger" title="1 jobs failed or had a failed dependency in evaluation #700 (click to inspect)">1 Failed</a>`,
		`<a href="/substitutes/cuirass.nonguix.org/evaluations/54" class="mono" title="nonguix commit ng54111122"><strong>#54</strong></a>`,
		`<a href="/substitutes/cuirass.nonguix.org/evaluations/54?status=succeeded" class="badge badge-ok"`,
		`<a href="/substitutes/cuirass.nonguix.org/evaluations/54?status=failed" class="badge badge-danger" title="1 nonguix jobs failed in evaluation #54 (click to inspect)">1 Failed</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("GET / missing %q", want)
		}
	}

	// 2. GET /substitutes/ci.guix.gnu.org/evaluations/699 shows Guix evaluation overview and all Guix packages (available + unavailable)
	req = httptest.NewRequest(http.MethodGet, "/substitutes/ci.guix.gnu.org/evaluations/699", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /substitutes/ci.guix.gnu.org/evaluations/699 status = %d, want 200", rec.Code)
	}
	evalBody := rec.Body.String()
	for _, want := range []string{
		`Evaluation #699`,
		`<span class="badge badge-muted mono">ci.guix.gnu.org</span>`,
		`Cuirass #699 &rarr;`,
		`Packages in Evaluation`,
		`<a href="/substitutes/ci.guix.gnu.org/evaluations/699?status=succeeded" onclick="return setUnavailableStatusFilter(event, 'succeeded')" class="badge badge-ok">3 Succeeded</a>`,
		`<a href="/substitutes/ci.guix.gnu.org/evaluations/699?status=failed" onclick="return setUnavailableStatusFilter(event, 'failed')" class="badge badge-danger">3 Failed</a>`,
		`<a href="/substitutes/ci.guix.gnu.org/evaluations/699?status=pending" onclick="return setUnavailableStatusFilter(event, 'pending')" class="badge badge-warn">0 Pending</a>`,
		`<a href="/substitutes/ci.guix.gnu.org/evaluations/699?status=not-built" onclick="return setUnavailableStatusFilter(event, 'not-built')" class="badge badge-muted">1 Not built</a>`,
		`<a id="eval-available-badge" href="/substitutes/ci.guix.gnu.org/evaluations/699?status=succeeded" onclick="return setUnavailableStatusFilter(event, 'succeeded')" class="badge badge-ok">3 available</a>`,
		`<a id="unavailable-total-badge" href="/substitutes/ci.guix.gnu.org/evaluations/699?status=unavailable" onclick="return setUnavailableStatusFilter(event, 'unavailable')" class="badge badge-danger">4 unavailable</a>`,
		`Total: 7 packages (3 available, 4 unavailable)`,
		`<span id="unavailable-eval-meta" class="mono" style="font-size:0.8rem;color:var(--muted);">guix #699 (ffff6666ee)</span>`,
		`<a href="/substitutes/cuirass.nonguix.org/evaluations/54" class="mono" title="nonguix commit ng54111122"><strong>#54</strong></a>`,
		`<a href="/packages/emacs">emacs</a>`,
		`<a href="/packages/libreoffice">libreoffice</a>`,
		`<a href="/packages/gcc-toolchain">gcc-toolchain</a> <span class="badge badge-muted" style="margin-left:0.35rem;">2 builds</span>`,
		`@14.2.0`,
		`@13.3.0`,
		`<a href="/packages/github-cli">github-cli</a>`,
		`<a href="/packages/ruby-mysql2">ruby-mysql2</a>`,
		`<a href="/packages/omitted-pkg">omitted-pkg</a>`,
		`Build #13 &rarr;`,
		`Build #14 &rarr;`,
		`<span class="badge badge-ok">Succeeded</span>`,
		`<span class="badge badge-warn">Failed (dependency)</span>`,
		`<span class="badge badge-danger">Failed</span>`,
		`<span class="badge badge-muted">Not built</span>`,
		`id="eval-pagination-wrap"`,
		`Showing 1-6 of 6`,
		`id="eval-page-input"`,
		`<script id="unavailable-pkgs-data" type="application/json">`,
		`"succeeded":["emacs","libreoffice"]`,
	} {
		if !strings.Contains(evalBody, want) {
			t.Fatalf("GET /substitutes/ci.guix.gnu.org/evaluations/699 missing %q", want)
		}
	}

	// 3. GET /substitutes/cuirass.nonguix.org/evaluations/54 shows Nonguix evaluation #54 scoped to cuirass.nonguix.org
	req = httptest.NewRequest(http.MethodGet, "/substitutes/cuirass.nonguix.org/evaluations/54", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /substitutes/cuirass.nonguix.org/evaluations/54 status = %d, want 200", rec.Code)
	}
	ngEvalBody := rec.Body.String()
	for _, want := range []string{
		`Evaluation #54`,
		`<span class="badge badge-muted mono">cuirass.nonguix.org</span>`,
		`https://cuirass.nonguix.org/eval/54`,
		`<a href="/substitutes/cuirass.nonguix.org/evaluations/54?status=succeeded" onclick="return setUnavailableStatusFilter(event, 'succeeded')" class="badge badge-ok">1 Succeeded</a>`,
		`<a href="/substitutes/cuirass.nonguix.org/evaluations/54?status=failed" onclick="return setUnavailableStatusFilter(event, 'failed')" class="badge badge-danger">1 Failed</a>`,
		`<a id="eval-available-badge" href="/substitutes/cuirass.nonguix.org/evaluations/54?status=succeeded" onclick="return setUnavailableStatusFilter(event, 'succeeded')" class="badge badge-ok">1 available</a>`,
		`<a id="unavailable-total-badge" href="/substitutes/cuirass.nonguix.org/evaluations/54?status=unavailable" onclick="return setUnavailableStatusFilter(event, 'unavailable')" class="badge badge-danger">1 unavailable</a>`,
		`Total: 2 packages (1 available, 1 unavailable)`,
		`<span id="unavailable-eval-meta" class="mono" style="font-size:0.8rem;color:var(--muted);">nonguix #54 (ng54111122)</span>`,
		`<a href="/substitutes/ci.guix.gnu.org/evaluations/699" class="mono"><strong>#699</strong></a>`,
		`<a href="/packages/linux?additional_channels=nonguix">linux</a>`,
		`<a href="/packages/nvidia-driver?additional_channels=nonguix">nvidia-driver</a>`,
		`Showing 1-2 of 2`,
		`"nonguix_succeeded":["linux"]`,
	} {
		if !strings.Contains(ngEvalBody, want) {
			t.Fatalf("GET /substitutes/cuirass.nonguix.org/evaluations/54 missing %q", want)
		}
	}

	// 4. GET /substitutes/ci.guix.gnu.org/evaluations/999999 and unknown substitute server return 404 Not Found
	req = httptest.NewRequest(http.MethodGet, "/substitutes/ci.guix.gnu.org/evaluations/999999", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /substitutes/ci.guix.gnu.org/evaluations/999999 status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Not Found in Cache") {
		t.Fatalf("GET /substitutes/ci.guix.gnu.org/evaluations/999999 missing 'Not Found in Cache'")
	}

	req = httptest.NewRequest(http.MethodGet, "/substitutes/unknown.example.org/evaluations/699", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /substitutes/unknown.example.org/evaluations/699 status = %d, want 404", rec.Code)
	}
}

func TestPackageStatusPage(t *testing.T) {
	st, err := store.Open(":memory:", "https://ci.guix.gnu.org", []string{"x86_64-linux"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.SetChannelServerURL("nonguix", "https://cuirass.nonguix.org")

	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	st.UpdateChannelEvaluations("guix", []store.EvaluationRecord{
		{ID: 700, Commit: "aaaa1111bbbb2222cccc3333dddd4444eeee5555", Timestamp: now},
		{ID: 699, Commit: "ffff6666eeee7777dddd8888cccc9999bbbb0000", Timestamp: now.Add(-time.Hour)},
		{ID: 698, Commit: "1111222233334444555566667777888899990000", Timestamp: now.Add(-2 * time.Hour)},
	}, 40)

	// Eval 700: 1 pending build (libreoffice), github-cli failed-dependency
	st.SetPullStatus("aaaa1111bbbb2222cccc3333dddd4444eeee5555", "x86_64-linux", 1700, true)
	st.UpsertChannelSystemEval("guix", 700, "aaaa1111bbbb2222cccc3333dddd4444eeee5555", "x86_64-linux", []cuirass.Job{
		{BuildID: 1, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
		{BuildID: 2, Name: "libreoffice.x86_64-linux", Status: cuirass.StatusScheduled, Version: "24.8"},
		{BuildID: 3, Name: "github-cli.x86_64-linux", Status: cuirass.StatusFailedDependency, Version: "2.52.0"},
	})

	// Eval 699 (Latest Ready): 0 pending builds, github-cli failed-dependency, gcc-toolchain@14 succeeded, gcc-toolchain@13 failed
	st.SetPullStatus("ffff6666eeee7777dddd8888cccc9999bbbb0000", "x86_64-linux", 1699, true)
	st.UpsertChannelSystemEval("guix", 699, "ffff6666eeee7777dddd8888cccc9999bbbb0000", "x86_64-linux", []cuirass.Job{
		{BuildID: 10, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
		{BuildID: 11, Name: "libreoffice.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "24.8"},
		{BuildID: 12, Name: "github-cli.x86_64-linux", Status: cuirass.StatusFailedDependency, Version: "2.52.0"},
		{BuildID: 13, Name: "gcc-toolchain.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "14.2.0"},
		{BuildID: 14, Name: "gcc-toolchain.x86_64-linux", Status: cuirass.StatusFailed, Version: "13.3.0"},
	})

	// Eval 698: github-cli succeeded!
	st.SetPullStatus("1111222233334444555566667777888899990000", "x86_64-linux", 1698, true)
	st.UpsertChannelSystemEval("guix", 698, "1111222233334444555566667777888899990000", "x86_64-linux", []cuirass.Job{
		{BuildID: 20, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
		{BuildID: 21, Name: "github-cli.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "2.52.0"},
	})

	st.UpdateChannelEvaluations("nonguix", []store.EvaluationRecord{
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
	st.UpsertChannelSystemEval("nonguix", 54, "ng54111122223333444455556666777788889999", "x86_64-linux", []cuirass.Job{
		{BuildID: 901, Name: "linux.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "6.12.1"},
		{BuildID: 902, Name: "nvidia-driver.x86_64-linux", Status: cuirass.StatusFailed, Version: "550.120"},
	})

	_ = st.Save()
	srv := NewServer(st, nil, nil)

	// 1. GET /packages/github-cli -> shows Failed (dependency) in Latest Ready (#699) and Newest Ready in #698
	req := httptest.NewRequest(http.MethodGet, "/packages/github-cli", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /packages/github-cli status = %d, want 200", rec.Code)
	}
	ghBody := rec.Body.String()
	for _, want := range []string{
		"github-cli - Package Substitute Status",
		"Status in Latest Ready Commit",
		`<span class="badge badge-warn">Failed (dependency)</span>`,
		`Newest Ready Commit for <code>github-cli</code>`,
		`guix time-machine --commit=1111222233334444555566667777888899990000 -- install github-cli`,
		`Newest Pkg Ready`,
		`<strong>1</strong> Succeeded &middot; <strong>2</strong> Failed`,
		`class="health-strip"`,
	} {
		if !strings.Contains(ghBody, want) {
			t.Fatalf("GET /packages/github-cli missing %q", want)
		}
	}

	// 2. GET /packages/gcc-toolchain -> multi-version package shows both @14.2.0 and @13.3.0
	req = httptest.NewRequest(http.MethodGet, "/packages/gcc-toolchain", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /packages/gcc-toolchain status = %d, want 200", rec.Code)
	}
	gccBody := rec.Body.String()
	for _, want := range []string{
		`@14.2.0`,
		`@13.3.0`,
		`Build #13 &rarr;`,
	} {
		if !strings.Contains(gccBody, want) {
			t.Fatalf("GET /packages/gcc-toolchain missing %q", want)
		}
	}

	// 3. GET /packages/nvidia-driver -> auto-detects nonguix channel
	req = httptest.NewRequest(http.MethodGet, "/packages/nvidia-driver", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /packages/nvidia-driver status = %d, want 200", rec.Code)
	}
	nvBody := rec.Body.String()
	for _, want := range []string{
		`<span class="badge badge-muted mono">nonguix</span>`,
		`id="main-nonguix-toggle" checked`,
		`/substitutes/cuirass.nonguix.org/evaluations/54`,
	} {
		if !strings.Contains(nvBody, want) {
			t.Fatalf("GET /packages/nvidia-driver missing %q", want)
		}
	}

	// 4. GET /packages/unknown-local-pkg -> returns 404 Not Found with helpful HTML
	req = httptest.NewRequest(http.MethodGet, "/packages/unknown-local-pkg", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /packages/unknown-local-pkg status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Not Tracked on CI") {
		t.Fatalf("GET /packages/unknown-local-pkg missing 'Not Tracked on CI' message")
	}
}

func TestStuckBuildDashboardAndDetailPages(t *testing.T) {
	st, err := store.Open(":memory:", "https://ci.guix.gnu.org", []string{"x86_64-linux"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	now := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)
	st.UpdateChannelEvaluations("guix", []store.EvaluationRecord{
		{ID: 800, Commit: "8008008008008008008008008008008008008008", Timestamp: now},
		{ID: 799, Commit: "7997997997997997997997997997997997997997", Timestamp: now.Add(-time.Hour)},
	}, 40)
	st.SetPullStatus("8008008008008008008008008008008008008008", "x86_64-linux", 1800, true)
	st.SetPullStatus("7997997997997997997997997997997997997997", "x86_64-linux", 1799, true)

	st.UpsertChannelSystemEval("guix", 800, "8008008008008008008008008008008008008008", "x86_64-linux", []cuirass.Job{
		{BuildID: 1, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded},
		{BuildID: 26974197, Name: "ath9k-htc-ar7010-firmware.x86_64-linux", Status: cuirass.StatusStuck},
	})
	st.UpsertChannelSystemEval("guix", 799, "7997997997997997997997997997997997997997", "x86_64-linux", []cuirass.Job{
		{BuildID: 2, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded},
		{BuildID: 3, Name: "ath9k-htc-ar7010-firmware.x86_64-linux", Status: cuirass.StatusSucceeded},
	})
	_ = st.Save()

	srv := NewServer(st, nil, nil)

	// 1. Main dashboard ("/") marks eval #800 as Ready / Latest Ready and hides Stuck badge in the main table.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	dashBody := rec.Body.String()
	if !strings.Contains(dashBody, "8008008008") || !strings.Contains(dashBody, "Latest Ready") {
		t.Fatalf("GET / expected eval #800 to be Latest Ready")
	}
	if strings.Contains(dashBody, "1 Stuck") {
		t.Fatalf("GET / should not display Stuck count badge in main evaluations table")
	}

	// 2. Evaluation detail page shows 1 Stuck badge, Stuck filter option, Stuck status badge, and Build #26974197 link.
	req = httptest.NewRequest(http.MethodGet, "/substitutes/ci.guix.gnu.org/evaluations/800", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET evaluation 800 status = %d, want 200", rec.Code)
	}
	evalBody := rec.Body.String()
	for _, want := range []string{
		`<a href="/substitutes/ci.guix.gnu.org/evaluations/800?status=succeeded" onclick="return setUnavailableStatusFilter(event, 'succeeded')" class="badge badge-ok">Ready</a>`,
		`<a href="/substitutes/ci.guix.gnu.org/evaluations/800?status=stuck" onclick="return setUnavailableStatusFilter(event, 'stuck')" class="badge badge-warn">1 Stuck</a>`,
		`<option value="stuck">Stuck</option>`,
		`<span class="badge badge-warn">Stuck</span>`,
		`Build #26974197 &rarr;`,
	} {
		if !strings.Contains(evalBody, want) {
			t.Fatalf("GET evaluation 800 missing %q", want)
		}
	}

	// 3. Package detail page shows Stuck in Latest Ready (#800), Build #26974197 link, health summary, and fallback to #799.
	req = httptest.NewRequest(http.MethodGet, "/packages/ath9k-htc-ar7010-firmware", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET package status = %d, want 200", rec.Code)
	}
	pkgBody := rec.Body.String()
	for _, want := range []string{
		`<span class="badge badge-warn">Stuck</span>`,
		`<strong>1</strong> Succeeded &middot; <strong>0</strong> Failed &middot; <strong>0</strong> Pending &middot; <strong>1</strong> Stuck &middot; <strong>0</strong> Not built`,
		`Build #26974197 &rarr;`,
		`guix time-machine --commit=7997997997997997997997997997997997997997 -- install ath9k-htc-ar7010-firmware`,
	} {
		if !strings.Contains(pkgBody, want) {
			t.Fatalf("GET /packages/ath9k-htc-ar7010-firmware missing %q", want)
		}
	}
}

func TestGuixJobsetEvaluationPageAndLinks(t *testing.T) {
	st, err := store.Open(":memory:", "https://ci.guix.gnu.org", []string{"x86_64-linux"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	now := time.Date(2026, 11, 14, 16, 0, 0, 0, time.UTC)
	commit801 := "8018018018018018018018018018018018018018"
	commit800 := "8008008008008008008008008008008008008008"

	st.UpdateChannelEvaluations("guix", []store.EvaluationRecord{
		{ID: 801, Commit: commit801, Timestamp: now},
		{ID: 800, Commit: commit800, Timestamp: now.Add(-time.Hour)},
	}, 40)
	st.UpdateChannelEvaluations(store.GuixJobsetChannel, []store.EvaluationRecord{
		{ID: 1801, Commit: commit801, Timestamp: now},
		{ID: 1800, Commit: commit800, Timestamp: now.Add(-time.Hour)},
	}, 40)

	st.SetPullStatus(commit801, "x86_64-linux", 1801, false)
	st.SetPullStatus(commit800, "x86_64-linux", 1800, true)

	st.UpsertChannelSystemEval("guix", 801, commit801, "x86_64-linux", []cuirass.Job{
		{BuildID: 10, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded},
		{BuildID: 11, Name: "guix.x86_64-linux", Status: cuirass.StatusSucceeded},
	})
	st.UpsertChannelSystemEval("guix", 800, commit800, "x86_64-linux", []cuirass.Job{
		{BuildID: 20, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded},
		{BuildID: 21, Name: "guix.x86_64-linux", Status: cuirass.StatusSucceeded},
	})
	st.UpsertChannelSystemEval(store.GuixJobsetChannel, 1801, commit801, "x86_64-linux", []cuirass.Job{
		{BuildID: 2801, Name: "guix.x86_64-linux", Status: cuirass.StatusScheduled},
	})
	st.UpsertChannelSystemEval(store.GuixJobsetChannel, 1800, commit800, "x86_64-linux", []cuirass.Job{
		{BuildID: 2800, Name: "guix.x86_64-linux", Status: cuirass.StatusSucceeded},
	})
	_ = st.Save()

	srv := NewServer(st, nil, nil)

	// 1. Main dashboard ("/") links guix/guix evaluations to /substitutes/ci.guix.gnu.org/evaluations/<id> rather than upstream /eval/<id>.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	dashBody := rec.Body.String()
	for _, want := range []string{
		`<a href="/substitutes/ci.guix.gnu.org/evaluations/1801" class="mono"><strong>#1801</strong></a>`,
		`href="/substitutes/ci.guix.gnu.org/evaluations/1801?status=pending"`,
		`>1 Pending</a>`,
		`<a href="/substitutes/ci.guix.gnu.org/evaluations/1800" class="mono"><strong>#1800</strong></a>`,
		`href="/substitutes/ci.guix.gnu.org/evaluations/1800?status=succeeded"`,
	} {
		if !strings.Contains(dashBody, want) {
			t.Fatalf("GET / missing %q", want)
		}
	}
	if strings.Contains(dashBody, "https://ci.guix.gnu.org/eval/1800") || strings.Contains(dashBody, "https://ci.guix.gnu.org/eval/1801") {
		t.Fatalf("GET / should not link guix/guix evaluations directly to upstream Cuirass /eval/<id>")
	}

	// 2. Package detail page ("/packages/emacs") and guix/master evaluation page ("/substitutes/.../800") also link guix/guix locally.
	req = httptest.NewRequest(http.MethodGet, "/packages/emacs", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /packages/emacs status = %d, want 200", rec.Code)
	}
	pkgBody := rec.Body.String()
	if !strings.Contains(pkgBody, `<a href="/substitutes/ci.guix.gnu.org/evaluations/1800" class="mono"><strong>#1800</strong></a>`) {
		t.Fatalf("GET /packages/emacs missing local link to guix/guix evaluation #1800")
	}
	if strings.Contains(pkgBody, "https://ci.guix.gnu.org/eval/1800") {
		t.Fatalf("GET /packages/emacs should not link guix/guix evaluation #1800 to upstream Cuirass")
	}

	req = httptest.NewRequest(http.MethodGet, "/substitutes/ci.guix.gnu.org/evaluations/800", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET evaluation 800 status = %d, want 200", rec.Code)
	}
	masterEvalBody := rec.Body.String()
	if !strings.Contains(masterEvalBody, `<a href="/substitutes/ci.guix.gnu.org/evaluations/1800" class="mono"><strong>#1800</strong></a>`) {
		t.Fatalf("GET evaluation 800 missing local link to guix/guix evaluation #1800")
	}
	if strings.Contains(masterEvalBody, "https://ci.guix.gnu.org/eval/1800") {
		t.Fatalf("GET evaluation 800 should not link guix/guix evaluation #1800 to upstream Cuirass")
	}

	// 3. GET /substitutes/ci.guix.gnu.org/evaluations/1800 renders the guix/guix evaluation details page.
	req = httptest.NewRequest(http.MethodGet, "/substitutes/ci.guix.gnu.org/evaluations/1800", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET evaluation 1800 status = %d, want 200", rec.Code)
	}
	pullReadyBody := rec.Body.String()
	for _, want := range []string{
		`Evaluation #1800`,
		`<span class="badge badge-ok">Latest Ready</span>`,
		`<span class="badge badge-muted mono">guix/guix</span>`,
		`guix/guix Job Breakdown`,
		`Paired guix/master Evaluation`,
		`<a href="/substitutes/ci.guix.gnu.org/evaluations/800" class="mono"><strong>#800</strong></a>`,
		`guix/guix #1800 (8008008008)`,
		`<a href="/packages/guix">guix</a>`,
		`https://ci.guix.gnu.org/eval/1800`,
		`Cuirass #1800 &rarr;`,
	} {
		if !strings.Contains(pullReadyBody, want) {
			t.Fatalf("GET evaluation 1800 missing %q", want)
		}
	}

	// 4. GET /substitutes/ci.guix.gnu.org/evaluations/1801 renders pending guix/guix evaluation with build link.
	req = httptest.NewRequest(http.MethodGet, "/substitutes/ci.guix.gnu.org/evaluations/1801", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET evaluation 1801 status = %d, want 200", rec.Code)
	}
	pullPendingBody := rec.Body.String()
	for _, want := range []string{
		`Evaluation #1801`,
		`1 Pending`,
		`Build #2801 &rarr;`,
		`<a href="/substitutes/ci.guix.gnu.org/evaluations/801" class="mono"><strong>#801</strong></a>`,
	} {
		if !strings.Contains(pullPendingBody, want) {
			t.Fatalf("GET evaluation 1801 missing %q", want)
		}
	}
}
