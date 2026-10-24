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

	// Default: max_pending = 0 and require_pull = true -> should pick Eval 298 (299 & 300 have pullReady=false)
	res, err := Resolve(snap, Query{System: "x86_64-linux", MaxPending: 0})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if res.EvaluationID != 298 || !res.PullReady {
		t.Fatalf("got eval %d (pullReady=%v), want 298 (true)", res.EvaluationID, res.PullReady)
	}

	// require_pull = false, max_pending = 0 -> should pick Eval 299 (1 behind master)
	res, err = Resolve(snap, Query{System: "x86_64-linux", MaxPending: 0, RequirePull: Bool(false)})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if res.EvaluationID != 299 || res.EvaluationsBehind != 1 {
		t.Fatalf("got eval %d (behind %d), want 299 (behind 1)", res.EvaluationID, res.EvaluationsBehind)
	}

	// require_pull = false, max_pending = 2 -> should pick Eval 300 (latest)
	res, err = Resolve(snap, Query{System: "x86_64-linux", MaxPending: 2, RequirePull: Bool(false)})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if res.EvaluationID != 300 || res.EvaluationsBehind != 0 {
		t.Fatalf("got eval %d, want 300", res.EvaluationID)
	}
}

func TestResolvePackageMode(t *testing.T) {
	st := buildTestStore(t)
	snap := st.Snapshot()

	// 1. Default (require_pull = true):
	// Even though emacs + git + gcc-toolchain@13 succeeded in 300, Eval 300 has pullReady=false.
	// Therefore default require_pull=true skips 300 and 299, picking Eval 298.
	res, err := Resolve(snap, Query{
		System:   "x86_64-linux",
		Packages: []string{"emacs", "git", "gcc-toolchain@13", "my-custom-local-pkg"},
	})
	if err != nil {
		t.Fatalf("Resolve package mode failed: %v", err)
	}
	if res.EvaluationID != 298 {
		t.Fatalf("got eval %d, want 298 (require_pull default true)", res.EvaluationID)
	}
	if len(res.UntrackedPackages) != 1 || res.UntrackedPackages[0] != "my-custom-local-pkg" {
		t.Fatalf("unexpected untracked packages: %v", res.UntrackedPackages)
	}

	// 2. With RequirePull = false:
	// emacs + git + gcc-toolchain@13 -> ready in Eval 300!
	res, err = Resolve(snap, Query{
		System:      "x86_64-linux",
		Packages:    []string{"emacs", "git", "gcc-toolchain@13"},
		RequirePull: Bool(false),
	})
	if err != nil {
		t.Fatalf("Resolve package mode failed: %v", err)
	}
	if res.EvaluationID != 300 {
		t.Fatalf("got eval %d, want 300", res.EvaluationID)
	}

	// gcc-toolchain (unversioned -> picks latest 14.2.0, pending in 300, succeeded in 299 with RequirePull=false)
	res, err = Resolve(snap, Query{
		System:      "x86_64-linux",
		Packages:    []string{"emacs", "gcc-toolchain"},
		RequirePull: Bool(false),
	})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if res.EvaluationID != 299 {
		t.Fatalf("got eval %d, want 299", res.EvaluationID)
	}

	// emacs + git + gcc-toolchain@14 -> git failed in 299, so even without pull check it picks 298
	res, err = Resolve(snap, Query{
		System:      "x86_64-linux",
		Packages:    []string{"emacs", "git", "gcc-toolchain@14"},
		RequirePull: Bool(false),
	})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if res.EvaluationID != 298 {
		t.Fatalf("got eval %d, want 298", res.EvaluationID)
	}

	// With allow_failed = true and RequirePull = false, emacs + git + gcc-toolchain@14 succeeds in 299
	res, err = Resolve(snap, Query{
		System:      "x86_64-linux",
		AllowFailed: true,
		RequirePull: Bool(false),
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
	if !strings.Contains(scm, "make-channel-introduction") || !strings.Contains(scm, "9edb3f66fd807b096b48283debdcddccfea34bad") {
		t.Fatalf("channels.scm missing introduction:\n%s", scm)
	}
}

func TestResolveWithNonguixChannel(t *testing.T) {
	st := buildTestStore(t)
	st.SetChannelServerURL("nonguix", "https://cuirass.nonguix.org")

	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	// Nonguix Eval 90 -> references commit300newest (which has pullReady=false and pending jobs)
	// Nonguix Eval 89 -> references commit299middle (which has pullReady=false and git failed)
	// Nonguix Eval 88 -> references commit298oldest (which has pullReady=true and 0 pending jobs)
	st.UpdateChannelEvaluations("nonguix", []store.EvaluationRecord{
		{
			ID:      90,
			Channel: "nonguix",
			Commit:  "ngcommit90newest0000000000000000000000000",
			Checkouts: map[string]string{
				"nonguix": "ngcommit90newest0000000000000000000000000",
				"guix":    "commit300newest00000000000000000000000000",
			},
			Timestamp: now,
		},
		{
			ID:      89,
			Channel: "nonguix",
			Commit:  "ngcommit89middle0000000000000000000000000",
			Checkouts: map[string]string{
				"nonguix": "ngcommit89middle0000000000000000000000000",
				"guix":    "commit299middle00000000000000000000000000",
			},
			Timestamp: now.Add(-1 * time.Hour),
		},
		{
			ID:      88,
			Channel: "nonguix",
			Commit:  "ngcommit88oldest0000000000000000000000000",
			Checkouts: map[string]string{
				"nonguix": "ngcommit88oldest0000000000000000000000000",
				"guix":    "commit298oldest00000000000000000000000000",
			},
			Timestamp: now.Add(-2 * time.Hour),
		},
	}, 40)

	// In Nonguix 90, linux succeeded, nvidia-driver pending
	st.UpsertChannelSystemEval("nonguix", 90, "ngcommit90newest0000000000000000000000000", "x86_64-linux", []cuirass.Job{
		{BuildID: 1, Name: "linux.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "6.12.1"},
		{BuildID: 2, Name: "nvidia-driver.x86_64-linux", Status: cuirass.StatusStarted, Version: "560.35"},
	})
	// In Nonguix 89, all succeeded
	st.UpsertChannelSystemEval("nonguix", 89, "ngcommit89middle0000000000000000000000000", "x86_64-linux", []cuirass.Job{
		{BuildID: 3, Name: "linux.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "6.12.1"},
		{BuildID: 4, Name: "nvidia-driver.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "560.35"},
	})
	// In Nonguix 88, all succeeded
	st.UpsertChannelSystemEval("nonguix", 88, "ngcommit88oldest0000000000000000000000000", "x86_64-linux", []cuirass.Job{
		{BuildID: 5, Name: "linux.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "6.12.1"},
		{BuildID: 6, Name: "nvidia-driver.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "560.35"},
	})
	st.PublishSnapshot()
	snap := st.Snapshot()

	// 1. Unsupported channel -> ErrUnsupportedChannel
	if _, err := Resolve(snap, Query{System: "x86_64-linux", AdditionalChannels: []string{"unknown-ch"}}); err == nil {
		t.Fatalf("expected ErrUnsupportedChannel, got nil")
	}

	// 2. Whole-evaluation mode with nonguix and default require_pull=true:
	// Eval 90 (guix 300) and Eval 89 (guix 299) have pullReady=false on Guix, so it must pick Eval 88 + Guix 298!
	res, err := Resolve(snap, Query{
		System:             "x86_64-linux",
		MaxPending:         0,
		AdditionalChannels: []string{"nonguix"},
	})
	if err != nil {
		t.Fatalf("Resolve whole-eval with nonguix failed: %v", err)
	}
	if res.EvaluationID != 298 || res.Commit != "commit298oldest00000000000000000000000000" {
		t.Fatalf("expected Guix eval 298, got %d (%s)", res.EvaluationID, res.Commit)
	}
	if len(res.AdditionalChannels) != 1 || res.AdditionalChannels[0].EvaluationID != 88 || res.AdditionalChannels[0].Commit != "ngcommit88oldest0000000000000000000000000" {
		t.Fatalf("expected Nonguix eval 88, got %+v", res.AdditionalChannels)
	}

	// 3. Package mode with require_pull=false and packages from both Guix ("emacs", "git") and Nonguix ("linux"):
	// In Eval 90 + Guix 300: emacs succeeded, git succeeded, and linux succeeded!
	resPkg, err := Resolve(snap, Query{
		System:             "x86_64-linux",
		RequirePull:        Bool(false),
		Packages:           []string{"emacs", "git", "linux", "local-pkg"},
		AdditionalChannels: []string{"nonguix"},
	})
	if err != nil {
		t.Fatalf("Resolve package mode with nonguix failed: %v", err)
	}
	if resPkg.EvaluationID != 300 || resPkg.AdditionalChannels[0].EvaluationID != 90 {
		t.Fatalf("expected Guix 300 + Nonguix 90, got Guix %d + Nonguix %d", resPkg.EvaluationID, resPkg.AdditionalChannels[0].EvaluationID)
	}
	if len(resPkg.UntrackedPackages) != 1 || resPkg.UntrackedPackages[0] != "local-pkg" {
		t.Fatalf("expected local-pkg in UntrackedPackages, got %v", resPkg.UntrackedPackages)
	}

	// 4. Verify FormatChannelsSCM includes both nonguix and guix channels
	scm := FormatChannelsSCM(res)
	if !strings.Contains(scm, "(name 'nonguix)") ||
		!strings.Contains(scm, `(commit "ngcommit88oldest0000000000000000000000000")`) ||
		!strings.Contains(scm, "897c1a470da759236cc11798f4e0a5f7d4d59fbc") ||
		!strings.Contains(scm, `(commit "commit298oldest00000000000000000000000000")`) {
		t.Fatalf("FormatChannelsSCM missing expected nonguix + guix channel definitions:\n%s", scm)
	}
}
