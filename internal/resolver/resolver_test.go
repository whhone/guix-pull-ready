package resolver

import (
	"strings"
	"testing"
	"time"

	"guix-evaluated/internal/cuirass"
	"guix-evaluated/internal/store"
)

func buildTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:", "https://ci.guix.gnu.org", []string{"x86_64-linux"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	evals := []store.EvaluationRecord{
		{ID: 300, Commit: "commit300newest00000000000000000000000000", Timestamp: now},
		{ID: 299, Commit: "commit299middle00000000000000000000000000", Timestamp: now.Add(-1 * time.Hour)},
		{ID: 298, Commit: "commit298oldest00000000000000000000000000", Timestamp: now.Add(-2 * time.Hour)},
	}
	st.UpdateEvaluations(evals, 40)

	// Eval 300: 2 pending jobs, emacs succeeded, gcc-toolchain@14 pending, gcc-toolchain@13 succeeded
	st.SetPullStatus(evals[0].Commit, "x86_64-linux", 1300, false)
	st.UpsertSystemEval(300, evals[0].Commit, "x86_64-linux", []cuirass.Job{
		{BuildID: 1, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
		{BuildID: 2, Name: "git.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "2.46.0"},
		{BuildID: 3, Name: "gcc-toolchain.x86_64-linux", Status: cuirass.StatusStarted, Version: "14.2.0"},
		{BuildID: 4, Name: "gcc-toolchain.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "13.3.0"},
		{BuildID: 5, Name: "blender.x86_64-linux", Status: cuirass.StatusScheduled, Version: "4.2.0"},
	})

	// Eval 299: 0 pending jobs, git failed, gcc-toolchain@14 succeeded, pull not ready
	st.SetPullStatus(evals[1].Commit, "x86_64-linux", 1299, false)
	st.UpsertSystemEval(299, evals[1].Commit, "x86_64-linux", []cuirass.Job{
		{BuildID: 11, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
		{BuildID: 12, Name: "git.x86_64-linux", Status: cuirass.StatusFailed, Version: "2.46.0"},
		{BuildID: 13, Name: "gcc-toolchain.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "14.2.0"},
		{BuildID: 14, Name: "gcc-toolchain.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "13.3.0"},
	})

	// Eval 298: 0 pending jobs, all succeeded, pull ready
	st.SetPullStatus(evals[2].Commit, "x86_64-linux", 1298, true)
	st.UpsertSystemEval(298, evals[2].Commit, "x86_64-linux", []cuirass.Job{
		{BuildID: 21, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
		{BuildID: 22, Name: "git.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "2.46.0"},
		{BuildID: 23, Name: "gcc-toolchain.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "14.2.0"},
		{BuildID: 24, Name: "base.gcc.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "14.2.0"},
	})

	if err := st.Save(); err != nil {
		t.Fatalf("st.Save: %v", err)
	}
	return st
}

func TestResolveWholeEvaluationMode(t *testing.T) {
	st := buildTestStore(t)
	snap := st.Snapshot()

	// Default max_pending = 0 -> should pick Eval 299 (1 behind master)
	res, err := Resolve(snap, Query{System: "x86_64-linux", MaxPending: 0})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if res.EvaluationID != 299 || res.EvaluationsBehind != 1 {
		t.Fatalf("got eval %d (behind %d), want 299 (behind 1)", res.EvaluationID, res.EvaluationsBehind)
	}

	// max_pending = 2 -> should pick Eval 300 (latest)
	res, err = Resolve(snap, Query{System: "x86_64-linux", MaxPending: 2})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if res.EvaluationID != 300 || res.EvaluationsBehind != 0 {
		t.Fatalf("got eval %d, want 300", res.EvaluationID)
	}

	// require_pull = true -> should pick Eval 298
	res, err = Resolve(snap, Query{System: "x86_64-linux", MaxPending: 0, RequirePull: true})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if res.EvaluationID != 298 || !res.PullReady {
		t.Fatalf("got eval %d (pullReady=%v), want 298 (true)", res.EvaluationID, res.PullReady)
	}
}

func TestResolvePackageMode(t *testing.T) {
	st := buildTestStore(t)
	snap := st.Snapshot()

	// emacs + git + gcc-toolchain@13 -> ready in Eval 300!
	res, err := Resolve(snap, Query{
		System:   "x86_64-linux",
		Packages: []string{"emacs", "git", "gcc-toolchain@13", "my-custom-local-pkg"},
	})
	if err != nil {
		t.Fatalf("Resolve package mode failed: %v", err)
	}
	if res.EvaluationID != 300 {
		t.Fatalf("got eval %d, want 300", res.EvaluationID)
	}
	if len(res.UntrackedPackages) != 1 || res.UntrackedPackages[0] != "my-custom-local-pkg" {
		t.Fatalf("unexpected untracked packages: %v", res.UntrackedPackages)
	}

	// gcc-toolchain (unversioned -> picks latest 14.2.0, which is pending in 300, succeeded in 299)
	res, err = Resolve(snap, Query{
		System:   "x86_64-linux",
		Packages: []string{"emacs", "gcc-toolchain"},
	})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if res.EvaluationID != 299 {
		t.Fatalf("got eval %d, want 299", res.EvaluationID)
	}

	// emacs + git + gcc-toolchain@14 -> git failed in 299, so without allow_failed it picks 298
	res, err = Resolve(snap, Query{
		System:   "x86_64-linux",
		Packages: []string{"emacs", "git", "gcc-toolchain@14"},
	})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if res.EvaluationID != 298 {
		t.Fatalf("got eval %d, want 298", res.EvaluationID)
	}

	// With allow_failed = true, emacs + git + gcc-toolchain@14 succeeds in 299
	res, err = Resolve(snap, Query{
		System:      "x86_64-linux",
		AllowFailed: true,
		Packages:    []string{"emacs", "git", "gcc-toolchain@14"},
	})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if res.EvaluationID != 299 {
		t.Fatalf("got eval %d, want 299", res.EvaluationID)
	}

	scm := FormatChannelsSCM(res)
	if !strings.Contains(scm, `(commit "commit299middle00000000000000000000000000")`) {
		t.Fatalf("unexpected channels.scm output:\n%s", scm)
	}
}
