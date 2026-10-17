package poller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"guix-evaluated/internal/cuirass"
	"guix-evaluated/internal/resolver"
	"guix-evaluated/internal/store"
)

func TestPollerIncrementalSyncAndBuildCache(t *testing.T) {
	var eval500Calls atomic.Int32
	var eval499Calls atomic.Int32
	var buildCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/evaluations":
			spec := r.URL.Query().Get("spec")
			if spec == "guix" {
				_, _ = w.Write([]byte(`[
					{"id":1500,"specification":"guix","timestamp":1729170000,"checkouts":[{"commit":"commit500","channel":"guix"}]},
					{"id":1499,"specification":"guix","timestamp":1729160000,"checkouts":[{"commit":"commit499","channel":"guix"}]}
				]`))
				return
			}
			_, _ = w.Write([]byte(`[
				{"id":500,"specification":"master","timestamp":1729170000,"checkouts":[{"commit":"commit500","channel":"guix"}]},
				{"id":499,"specification":"master","timestamp":1729160000,"checkouts":[{"commit":"commit499","channel":"guix"}]}
			]`))

		case "/api/jobs":
			evalID := r.URL.Query().Get("evaluation")
			switch evalID {
			case "500":
				eval500Calls.Add(1)
				// Eval 500 has 1 pending job (gcc-toolchain@14) and 1 succeeded job (gcc-toolchain@13)
				_, _ = w.Write([]byte(`[
					{"build":901,"status":0,"name":"emacs.x86_64-linux"},
					{"build":902,"status":-1,"name":"gcc-toolchain.x86_64-linux"},
					{"build":903,"status":0,"name":"gcc-toolchain.x86_64-linux"}
				]`))
			case "499":
				eval499Calls.Add(1)
				// Eval 499 has 0 pending jobs (complete!)
				_, _ = w.Write([]byte(`[
					{"build":801,"status":0,"name":"emacs.x86_64-linux"},
					{"build":802,"status":0,"name":"gcc-toolchain.x86_64-linux"}
				]`))
			case "1500":
				_, _ = w.Write([]byte(`[{"build":2500,"status":-2,"name":"guix.x86_64-linux"}]`))
			case "1499":
				_, _ = w.Write([]byte(`[{"build":2499,"status":0,"name":"guix.x86_64-linux"}]`))
			}

		case "/build/902":
			buildCalls.Add(1)
			_, _ = w.Write([]byte(`{"id":902,"derivation":"/gnu/store/aaaa-gcc-toolchain-14.2.0.drv","buildstatus":-1,"finished":0}`))
		case "/build/903":
			buildCalls.Add(1)
			_, _ = w.Write([]byte(`{"id":903,"derivation":"/gnu/store/bbbb-gcc-toolchain-13.3.0.drv","buildstatus":0,"finished":1}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := cuirass.NewClient(srv.URL, cuirass.WithRetry(2, 5*time.Millisecond))
	st, err := store.Open(":memory:", srv.URL, []string{"x86_64-linux"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	p := New(client, st, Config{
		Systems:      []string{"x86_64-linux"},
		Limit:        10,
		PollInterval: time.Minute,
	})

	ctx := context.Background()
	if err := p.SyncOnce(ctx); err != nil {
		t.Fatalf("first SyncOnce failed: %v", err)
	}

	if eval500Calls.Load() != 1 || eval499Calls.Load() != 1 {
		t.Fatalf("expected 1 call each to eval 500 and 499, got %d and %d", eval500Calls.Load(), eval499Calls.Load())
	}

	// Second SyncOnce: eval 499 is complete (0 pending), so only eval 500 should be re-polled!
	if err := p.SyncOnce(ctx); err != nil {
		t.Fatalf("second SyncOnce failed: %v", err)
	}
	if eval499Calls.Load() != 1 {
		t.Fatalf("expected eval 499 not to be re-fetched, got %d calls", eval499Calls.Load())
	}
	if eval500Calls.Load() != 2 {
		t.Fatalf("expected eval 500 to be re-fetched (2 calls), got %d", eval500Calls.Load())
	}

	// Resolve gcc-toolchain@13 -> triggers lazy build version lookup for build 902 and 903
	p.ResolveMissingVersions(ctx, "x86_64-linux", []string{"gcc-toolchain@13"})
	res, err := resolver.Resolve(st.Snapshot(), resolver.Query{
		System:   "x86_64-linux",
		Packages: []string{"emacs", "gcc-toolchain@13"},
	})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if res.Commit != "commit500" {
		t.Fatalf("expected commit500 for gcc-toolchain@13, got %s", res.Commit)
	}
	if buildCalls.Load() != 2 {
		t.Fatalf("expected 2 build calls, got %d", buildCalls.Load())
	}
}
