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
				_, _ = w.Write([]byte(`[{"build":2500,"status":0,"name":"guix.x86_64-linux"}]`))
			case "1499":
				_, _ = w.Write([]byte(`[{"build":2499,"status":0,"name":"guix.x86_64-linux"}]`))
			}

		case "/build/902":
			buildCalls.Add(1)
			_, _ = w.Write([]byte(`{"id":902,"derivation":"/gnu/store/aaaa-gcc-toolchain-14.2.0.drv","buildstatus":-1,"finished":0}`))
		case "/build/903":
			buildCalls.Add(1)
			_, _ = w.Write([]byte(`{"id":903,"derivation":"/gnu/store/bbbb-gcc-toolchain-13.3.0.drv","buildstatus":0,"finished":1}`))
		case "/commits/commit500":
			_, _ = w.Write([]byte(`{"sha":"commit500","commit":{"committer":{"date":"2026-10-17T10:00:00Z"}}}`))
		case "/commits/commit499":
			_, _ = w.Write([]byte(`{"sha":"commit499","commit":{"committer":{"date":"2026-10-17T09:00:00Z"}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := cuirass.NewClient(srv.URL,
		cuirass.WithRetry(2, 5*time.Millisecond),
		cuirass.WithCommitBaseURL(srv.URL+"/commits"),
	)
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
	wantCommitTime := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	if !res.EvaluationDate.Equal(wantCommitTime) {
		t.Fatalf("expected EvaluationDate = %v (from commit metadata), got %v", wantCommitTime, res.EvaluationDate)
	}
	if buildCalls.Load() != 2 {
		t.Fatalf("expected 2 build calls, got %d", buildCalls.Load())
	}
}

func TestPollerFairRoundRobinRefresh(t *testing.T) {
	evalCalls := make(map[string]*atomic.Int32)
	for _, id := range []string{"101", "102", "103", "104"} {
		evalCalls[id] = &atomic.Int32{}
	}

	var eval101Finished atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/evaluations":
			if r.URL.Query().Get("spec") == "guix" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[
				{"id":104,"specification":"master","timestamp":1729170040,"checkouts":[{"commit":"commit104","channel":"guix"}]},
				{"id":103,"specification":"master","timestamp":1729170030,"checkouts":[{"commit":"commit103","channel":"guix"}]},
				{"id":102,"specification":"master","timestamp":1729170020,"checkouts":[{"commit":"commit102","channel":"guix"}]},
				{"id":101,"specification":"master","timestamp":1729170010,"checkouts":[{"commit":"commit101","channel":"guix"}]}
			]`))
		case "/api/jobs":
			evalID := r.URL.Query().Get("evaluation")
			if counter, ok := evalCalls[evalID]; ok {
				counter.Add(1)
			}
			if evalID == "101" && eval101Finished.Load() {
				// Evaluation 101 completed!
				_, _ = w.Write([]byte(`[{"build":1001,"status":0,"name":"hello.x86_64-linux"}]`))
				return
			}
			// Otherwise, return 1 pending job
			_, _ = w.Write([]byte(`[{"build":1000,"status":-1,"name":"hello.x86_64-linux"}]`))
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
		Systems:          []string{"x86_64-linux"},
		Limit:            10,
		PollInterval:     time.Minute,
		MaxActiveRefresh: 2,
	})

	ctx := context.Background()

	// Pass 1: all 4 are uncached, so all 4 should be fetched.
	if err := p.SyncOnce(ctx); err != nil {
		t.Fatalf("pass 1 failed: %v", err)
	}
	for id, counter := range evalCalls {
		if counter.Load() != 1 {
			t.Fatalf("expected 1 call for eval %s on pass 1, got %d", id, counter.Load())
		}
	}

	// Pass 2: all 4 are cached and incomplete. MaxActiveRefresh = 2 refreshes 2 evaluations (104 and 103).
	if err := p.SyncOnce(ctx); err != nil {
		t.Fatalf("pass 2 failed: %v", err)
	}
	// Check that exactly 2 evaluations were refreshed on pass 2
	pass2Refreshed := 0
	for _, counter := range evalCalls {
		if counter.Load() == 2 {
			pass2Refreshed++
		}
	}
	if pass2Refreshed != 2 {
		t.Fatalf("expected exactly 2 evaluations refreshed on pass 2, got %d", pass2Refreshed)
	}

	// Pass 3: round-robin guarantees the remaining 2 evaluations (102 and 101) are refreshed!
	eval101Finished.Store(true) // 101 finishes builds during this poll
	if err := p.SyncOnce(ctx); err != nil {
		t.Fatalf("pass 3 failed: %v", err)
	}
	// Now all 4 evaluations must have been refreshed at least twice across the cycles.
	for id, counter := range evalCalls {
		if counter.Load() < 2 {
			t.Fatalf("eval %s was starved! Total calls: %d", id, counter.Load())
		}
	}

	// Eval 101 should now be marked complete in the store!
	if !st.IsEvalSystemComplete(101, "x86_64-linux") {
		t.Fatalf("expected eval 101 to be complete in store")
	}

	// Pass 4: eval 101 is complete, so it must NOT be refreshed again!
	eval101Before := evalCalls["101"].Load()
	if err := p.SyncOnce(ctx); err != nil {
		t.Fatalf("pass 4 failed: %v", err)
	}
	if evalCalls["101"].Load() != eval101Before {
		t.Fatalf("completed eval 101 was re-polled! expected %d, got %d", eval101Before, evalCalls["101"].Load())
	}
}

func TestPollerGuixPullReadyRefreshedForCompletedMasterEval(t *testing.T) {
	var masterCalls atomic.Int32
	var guixPullCalls atomic.Int32
	var guixJobSucceeded atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/evaluations":
			spec := r.URL.Query().Get("spec")
			if spec == "guix" {
				_, _ = w.Write([]byte(`[
					{"id":1700,"specification":"guix","timestamp":1729170000,"checkouts":[{"commit":"commit700","channel":"guix"}]}
				]`))
				return
			}
			_, _ = w.Write([]byte(`[
				{"id":700,"specification":"master","timestamp":1729170000,"checkouts":[{"commit":"commit700","channel":"guix"}]}
			]`))

		case "/api/jobs":
			evalID := r.URL.Query().Get("evaluation")
			switch evalID {
			case "700":
				masterCalls.Add(1)
				// Master evaluation is already complete (0 pending jobs)
				_, _ = w.Write([]byte(`[
					{"build":701,"status":0,"name":"emacs.x86_64-linux"}
				]`))
			case "1700":
				guixPullCalls.Add(1)
				if guixJobSucceeded.Load() {
					_, _ = w.Write([]byte(`[{"build":2700,"status":0,"name":"guix.x86_64-linux"}]`))
				} else {
					// Still in progress / pending in cycle 1
					_, _ = w.Write([]byte(`[{"build":2700,"status":-1,"name":"guix.x86_64-linux"}]`))
				}
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := cuirass.NewClient(srv.URL)
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

	// Cycle 1: master is complete, but guix pull job is still pending (-1)
	if err := p.SyncOnce(ctx); err != nil {
		t.Fatalf("cycle 1 failed: %v", err)
	}

	if masterCalls.Load() != 1 {
		t.Fatalf("expected 1 call to master eval 700, got %d", masterCalls.Load())
	}
	if guixPullCalls.Load() != 1 {
		t.Fatalf("expected 1 call to guix pull eval 1700, got %d", guixPullCalls.Load())
	}
	if !st.IsEvalSystemComplete(700, "x86_64-linux") {
		t.Fatalf("expected master eval 700 to be marked complete")
	}

	snap1 := st.Snapshot()
	data1 := snap1.ByEvalSystem[store.EvalSystemKey{EvalID: 700, System: "x86_64-linux"}]
	if data1 == nil || data1.PullReady {
		t.Fatalf("expected data1 to exist and PullReady to be false in cycle 1, got %+v", data1)
	}

	// Cycle 2: guix pull job finishes and succeeds (status 0).
	// Master eval 700 is complete, so master jobs must NOT be re-polled,
	// but guix pull status must be polled and updated to ready!
	guixJobSucceeded.Store(true)
	if err := p.SyncOnce(ctx); err != nil {
		t.Fatalf("cycle 2 failed: %v", err)
	}

	if masterCalls.Load() != 1 {
		t.Fatalf("expected master eval 700 NOT to be re-fetched, but got %d calls", masterCalls.Load())
	}
	if guixPullCalls.Load() != 2 {
		t.Fatalf("expected guix pull eval 1700 to be polled again in cycle 2, got %d calls", guixPullCalls.Load())
	}

	snap2 := st.Snapshot()
	data2 := snap2.ByEvalSystem[store.EvalSystemKey{EvalID: 700, System: "x86_64-linux"}]
	if data2 == nil || !data2.PullReady {
		t.Fatalf("expected data2 to have PullReady=true in cycle 2, got %+v", data2)
	}

	cachedPull, ok := st.GetPullStatus("commit700", "x86_64-linux")
	if !ok || !cachedPull.Ready {
		t.Fatalf("expected cached pull status to be ready, got ok=%v, status=%+v", ok, cachedPull)
	}

	// Cycle 3: once ready, guix pull job must NOT be polled again!
	if err := p.SyncOnce(ctx); err != nil {
		t.Fatalf("cycle 3 failed: %v", err)
	}
	if guixPullCalls.Load() != 2 {
		t.Fatalf("expected guix pull eval 1700 NOT to be polled after succeeding, got %d calls", guixPullCalls.Load())
	}
}

func TestPollerNonguixCoordinationAndVersionResolution(t *testing.T) {
	// Mock official Guix Cuirass server
	guixSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/evaluations":
			spec := r.URL.Query().Get("spec")
			if spec == "guix" {
				_, _ = w.Write([]byte(`[
					{"id":1800,"specification":"guix","timestamp":1729170000,"checkouts":[{"commit":"guix800","channel":"guix"}]},
					{"id":1799,"specification":"guix","timestamp":1729160000,"checkouts":[{"commit":"guix799","channel":"guix"}]}
				]`))
				return
			}
			_, _ = w.Write([]byte(`[
				{"id":800,"specification":"master","timestamp":1729170000,"checkouts":[{"commit":"guix800","channel":"guix"}]},
				{"id":799,"specification":"master","timestamp":1729160000,"checkouts":[{"commit":"guix799","channel":"guix"}]}
			]`))
		case "/api/jobs":
			switch r.URL.Query().Get("evaluation") {
			case "800":
				// Guix 800 has 1 pending build
				_, _ = w.Write([]byte(`[
					{"build":101,"status":0,"name":"emacs.x86_64-linux"},
					{"build":102,"status":-1,"name":"blender.x86_64-linux"}
				]`))
			case "799":
				// Guix 799 has 0 pending builds
				_, _ = w.Write([]byte(`[
					{"build":103,"status":0,"name":"emacs.x86_64-linux"},
					{"build":104,"status":0,"name":"blender.x86_64-linux"}
				]`))
			case "1800", "1799":
				_, _ = w.Write([]byte(`[{"build":201,"status":0,"name":"guix.x86_64-linux"}]`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer guixSrv.Close()

	// Mock Nonguix Cuirass server (note: uses overlapping build IDs 101, 102!)
	ngSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/evaluations":
			_, _ = w.Write([]byte(`[
				{"id":50,"specification":"nonguix","timestamp":1729170500,"checkouts":[{"commit":"ng50","channel":"nonguix"},{"commit":"guix800","channel":"guix"}]},
				{"id":49,"specification":"nonguix","timestamp":1729160500,"checkouts":[{"commit":"ng49","channel":"nonguix"},{"commit":"guix799","channel":"guix"}]}
			]`))
		case "/api/jobs":
			switch r.URL.Query().Get("evaluation") {
			case "50":
				// Multi-version linux in eval 50: 6.12 is pending (-1), 6.6 is succeeded (0)
				_, _ = w.Write([]byte(`[
					{"build":101,"status":-1,"name":"linux.x86_64-linux"},
					{"build":102,"status":0,"name":"linux.x86_64-linux"},
					{"build":103,"status":0,"name":"linux-firmware.x86_64-linux"}
				]`))
			case "49":
				// All succeeded in eval 49
				_, _ = w.Write([]byte(`[
					{"build":91,"status":0,"name":"linux.x86_64-linux"},
					{"build":92,"status":0,"name":"linux-firmware.x86_64-linux"}
				]`))
			}
		case "/build/101":
			_, _ = w.Write([]byte(`{"id":101,"derivation":"/gnu/store/xxxx-linux-6.12.1.drv"}`))
		case "/build/102":
			_, _ = w.Write([]byte(`{"id":102,"derivation":"/gnu/store/yyyy-linux-6.6.62.drv"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ngSrv.Close()

	guixClient := cuirass.NewClient(guixSrv.URL)
	ngClient := cuirass.NewClient(ngSrv.URL)
	st, err := store.Open(":memory:", guixSrv.URL, []string{"x86_64-linux"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	p := New(guixClient, st, Config{
		Systems:      []string{"x86_64-linux"},
		Limit:        10,
		PollInterval: time.Minute,
		AdditionalChannels: []ChannelConfig{
			{Name: "nonguix", Spec: "nonguix", Client: ngClient},
		},
	})

	ctx := context.Background()
	if err := p.SyncOnce(ctx); err != nil {
		t.Fatalf("SyncOnce failed: %v", err)
	}

	// 1. Whole-evaluation mode with nonguix:
	// Eval 50 (guix800) has pending builds on both nonguix and guix, so it should pick Eval 49 + Guix 799.
	res, err := resolver.Resolve(st.Snapshot(), resolver.Query{
		System:             "x86_64-linux",
		MaxPending:         0,
		AdditionalChannels: []string{"nonguix"},
	})
	if err != nil {
		t.Fatalf("Resolve whole-eval with nonguix failed: %v", err)
	}
	if res.Commit != "guix799" || len(res.AdditionalChannels) != 1 || res.AdditionalChannels[0].Commit != "ng49" {
		t.Fatalf("unexpected coordinated whole-eval result: %+v", res)
	}

	// 2. Package mode with versioned nonguix package "linux@6.6" and guix package "emacs":
	// In eval 50 (guix800), emacs succeeded and linux@6.6 succeeded!
	p.ResolveMissingVersions(ctx, "x86_64-linux", []string{"emacs", "linux@6.6"}, "nonguix")
	resPkg, err := resolver.Resolve(st.Snapshot(), resolver.Query{
		System:             "x86_64-linux",
		Packages:           []string{"emacs", "linux@6.6"},
		AdditionalChannels: []string{"nonguix"},
	})
	if err != nil {
		t.Fatalf("Resolve package mode with nonguix failed: %v", err)
	}
	if resPkg.Commit != "guix800" || len(resPkg.AdditionalChannels) != 1 || resPkg.AdditionalChannels[0].Commit != "ng50" {
		t.Fatalf("expected guix800 + ng50 for emacs + linux@6.6, got %+v", resPkg)
	}
}

func TestPollerReconcileStuckPendingBuild(t *testing.T) {
	var eval600Calls atomic.Int32
	var buildCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/evaluations":
			spec := r.URL.Query().Get("spec")
			if spec == "guix" {
				_, _ = w.Write([]byte(`[{"id":1600,"specification":"guix","timestamp":1729170000,"checkouts":[{"commit":"commit600","channel":"guix"}]}]`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":600,"specification":"master","timestamp":1729170000,"checkouts":[{"commit":"commit600","channel":"guix"}]}]`))

		case "/api/jobs":
			evalID := r.URL.Query().Get("evaluation")
			switch evalID {
			case "600":
				eval600Calls.Add(1)
				// /api/jobs reports gr-iqbal as status: -3 (scheduled/pending) with build 26715933
				_, _ = w.Write([]byte(`[
					{"build":1001,"status":0,"name":"emacs.x86_64-linux"},
					{"build":26715933,"status":-3,"name":"gr-iqbal.x86_64-linux"}
				]`))
			case "1600":
				_, _ = w.Write([]byte(`[{"build":3600,"status":0,"name":"guix.x86_64-linux"}]`))
			}

		case "/build/26715933":
			buildCalls.Add(1)
			// /build/26715933 shows finished: 1 and buildstatus: 0 (succeeded under another jobset)
			_, _ = w.Write([]byte(`{
				"id": 26715933,
				"finished": 1,
				"buildstatus": 0,
				"nixname": "gr-iqbal-0.38.3",
				"derivation": "/gnu/store/nlkvg0xk2c2lzxs97nd5x2l8s6zqjwaz-gr-iqbal-0.38.3.drv"
			}`))

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
		Systems:             []string{"x86_64-linux"},
		Limit:               10,
		PollInterval:        time.Minute,
		MaxReconcilePending: 10,
	})

	ctx := context.Background()
	if err := p.SyncOnce(ctx); err != nil {
		t.Fatalf("first SyncOnce failed: %v", err)
	}

	if eval600Calls.Load() != 1 {
		t.Fatalf("expected 1 call to eval 600, got %d", eval600Calls.Load())
	}
	if buildCalls.Load() != 1 {
		t.Fatalf("expected 1 call to /build/26715933, got %d", buildCalls.Load())
	}

	snap := st.Snapshot()
	data := snap.ByEvalSystem[store.EvalSystemKey{EvalID: 600, System: "x86_64-linux"}]
	if data == nil {
		t.Fatalf("expected cached data for eval 600")
	}
	if data.PendingJobs != 0 {
		t.Fatalf("expected PendingJobs == 0 after reconciliation, got %d", data.PendingJobs)
	}
	if data.SucceededJobs != 2 {
		t.Fatalf("expected SucceededJobs == 2 after reconciliation, got %d", data.SucceededJobs)
	}
	if !data.Complete {
		t.Fatalf("expected Complete == true after reconciliation")
	}

	// Second SyncOnce: evaluation 600 is complete, so it should not be re-polled!
	if err := p.SyncOnce(ctx); err != nil {
		t.Fatalf("second SyncOnce failed: %v", err)
	}
	if eval600Calls.Load() != 1 {
		t.Fatalf("expected eval 600 not to be re-fetched after completion, got %d calls", eval600Calls.Load())
	}
	if buildCalls.Load() != 1 {
		t.Fatalf("expected /build/26715933 not to be re-fetched after completion, got %d calls", buildCalls.Load())
	}

	// Verify resolution in whole-eval mode
	res, err := resolver.Resolve(st.Snapshot(), resolver.Query{
		System:     "x86_64-linux",
		MaxPending: 0,
	})
	if err != nil {
		t.Fatalf("Resolve whole-eval failed: %v", err)
	}
	if res.Commit != "commit600" {
		t.Fatalf("expected commit600, got %s", res.Commit)
	}

	// Verify resolution in package mode for gr-iqbal
	resPkg, err := resolver.Resolve(st.Snapshot(), resolver.Query{
		System:   "x86_64-linux",
		Packages: []string{"gr-iqbal"},
	})
	if err != nil {
		t.Fatalf("Resolve package mode for gr-iqbal failed: %v", err)
	}
	if resPkg.Commit != "commit600" {
		t.Fatalf("expected commit600 for gr-iqbal, got %s", resPkg.Commit)
	}
}

func TestPollerIncrementalBatchReconciliation(t *testing.T) {
	var evalCalls atomic.Int32
	var buildCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/evaluations":
			spec := r.URL.Query().Get("spec")
			if spec == "guix" {
				_, _ = w.Write([]byte(`[{"id":1700,"specification":"guix","timestamp":1729170000,"checkouts":[{"commit":"commit700","channel":"guix"}]}]`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":700,"specification":"master","timestamp":1729170000,"checkouts":[{"commit":"commit700","channel":"guix"}]}]`))

		case "/api/jobs":
			eid := r.URL.Query().Get("evaluation")
			if eid == "1700" {
				_, _ = w.Write([]byte(`[{"build":3700,"status":0,"name":"guix.x86_64-linux"}]`))
				return
			}
			if eid == "700" {
				evalCalls.Add(1)
				// Evaluation 700 has 5 pending jobs
				_, _ = w.Write([]byte(`[
					{"build":7001,"status":-1,"name":"pkg1.x86_64-linux"},
					{"build":7002,"status":-1,"name":"pkg2.x86_64-linux"},
					{"build":7003,"status":-1,"name":"pkg3.x86_64-linux"},
					{"build":7004,"status":-1,"name":"pkg4.x86_64-linux"},
					{"build":7005,"status":-1,"name":"pkg5.x86_64-linux"}
				]`))
				return
			}

		default:
			if len(r.URL.Path) > 7 && r.URL.Path[:7] == "/build/" {
				buildCalls.Add(1)
				_, _ = w.Write([]byte(`{"finished": 1, "buildstatus": 0}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := cuirass.NewClient(srv.URL, cuirass.WithRetry(2, 5*time.Millisecond))
	st, err := store.Open(":memory:", srv.URL, []string{"x86_64-linux"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	// Batch size of 3 with 5 pending jobs (within 2x limit = 6)
	p := New(client, st, Config{
		Systems:             []string{"x86_64-linux"},
		Limit:               10,
		PollInterval:        time.Minute,
		MaxReconcilePending: 3,
	})

	ctx := context.Background()

	// Pass 1: should reconcile 3 jobs, leaving 2 pending
	if err := p.SyncOnce(ctx); err != nil {
		t.Fatalf("pass 1 SyncOnce failed: %v", err)
	}
	snap := st.Snapshot()
	data := snap.ByEvalSystem[store.EvalSystemKey{EvalID: 700, System: "x86_64-linux"}]
	if data.PendingJobs != 2 {
		t.Fatalf("expected 2 pending jobs after pass 1, got %d", data.PendingJobs)
	}
	if data.SucceededJobs != 3 {
		t.Fatalf("expected 3 succeeded jobs after pass 1, got %d", data.SucceededJobs)
	}
	if data.Complete {
		t.Fatalf("expected eval 700 not complete after pass 1")
	}

	// Pass 2: should reconcile remaining 2 jobs, reaching 0 pending and Complete!
	if err := p.SyncOnce(ctx); err != nil {
		t.Fatalf("pass 2 SyncOnce failed: %v", err)
	}
	snap = st.Snapshot()
	data = snap.ByEvalSystem[store.EvalSystemKey{EvalID: 700, System: "x86_64-linux"}]
	if data.PendingJobs != 0 {
		t.Fatalf("expected 0 pending jobs after pass 2, got %d", data.PendingJobs)
	}
	if data.SucceededJobs != 5 {
		t.Fatalf("expected 5 succeeded jobs after pass 2, got %d", data.SucceededJobs)
	}
	if !data.Complete {
		t.Fatalf("expected eval 700 complete after pass 2")
	}

	// Pass 3: completed evaluation should not be polled anymore
	if err := p.SyncOnce(ctx); err != nil {
		t.Fatalf("pass 3 SyncOnce failed: %v", err)
	}
	if evalCalls.Load() != 2 {
		t.Fatalf("expected exactly 2 calls to eval 700, got %d", evalCalls.Load())
	}
}




