package store

import (
	"path/filepath"
	"testing"
	"time"

	"guix-evaluated/internal/cuirass"
)

func TestStorePersistAndReload(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "guix-sync.db")

	st, err := Open(dbPath, "https://ci.guix.gnu.org", []string{"x86_64-linux"})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	ts := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	st.UpdateEvaluations([]EvaluationRecord{
		{ID: 1001, Commit: "commit1001", Timestamp: ts},
	}, 40)
	st.SetPullStatus("commit1001", "x86_64-linux", 2001, true)
	st.SetBuildNixNames(map[int64]string{555: "emacs-29.4"})
	st.UpsertSystemEval(1001, "commit1001", "x86_64-linux", []cuirass.Job{
		{BuildID: 555, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
	})

	if err := st.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Re-open from disk
	st2, err := Open(dbPath, "https://ci.guix.gnu.org", []string{"x86_64-linux"})
	if err != nil {
		t.Fatalf("Open reload failed: %v", err)
	}

	if !st2.IsEvalSystemComplete(1001, "x86_64-linux") {
		t.Fatalf("expected evaluation 1001 to be marked complete after reload")
	}
	if nix, ok := st2.GetBuildNixName(555); !ok || nix != "emacs-29.4" {
		t.Fatalf("expected build 555 nixname emacs-29.4, got %q (ok=%v)", nix, ok)
	}
	snap := st2.Snapshot()
	if len(snap.Evaluations) != 1 || snap.Evaluations[0].Commit != "commit1001" {
		t.Fatalf("unexpected evaluations after reload: %+v", snap.Evaluations)
	}
}

func TestStoreMultiChannelPersistAndRetention(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "guix-sync-multi.db")

	st, err := Open(dbPath, "https://ci.guix.gnu.org", []string{"x86_64-linux"})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	st.SetChannelServerURL("nonguix", "https://cuirass.nonguix.org")

	ts := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	st.UpdateEvaluations([]EvaluationRecord{
		{ID: 1002, Commit: "guixcommit1002", Timestamp: ts},
		{ID: 1001, Commit: "guixcommit1001", Timestamp: ts.Add(-time.Hour)},
	}, 40)
	st.SetPullStatus("guixcommit1001", "x86_64-linux", 2001, true)
	st.UpsertSystemEval(1001, "guixcommit1001", "x86_64-linux", []cuirass.Job{
		{BuildID: 555, Name: "emacs.x86_64-linux", Status: cuirass.StatusSucceeded, Version: "29.4"},
	})

	// Nonguix eval 50 references guixcommit1001 and uses the same BuildID 555 for a different package ("linux").
	st.UpdateChannelEvaluations("nonguix", []EvaluationRecord{
		{
			ID:      50,
			Channel: "nonguix",
			Commit:  "ngcommit50",
			Checkouts: map[string]string{
				"nonguix": "ngcommit50",
				"guix":    "guixcommit1001",
			},
			Timestamp: ts,
		},
	}, 40)
	st.SetChannelBuildNixNames("nonguix", map[int64]string{555: "linux-6.12.1"})
	st.UpsertChannelSystemEval("nonguix", 50, "ngcommit50", "x86_64-linux", []cuirass.Job{
		{BuildID: 555, Name: "linux.x86_64-linux", Status: cuirass.StatusSucceeded},
		{BuildID: 556, Name: "linux.x86_64-linux", Status: cuirass.StatusStarted},
	})

	// Update Guix evaluations with maxRetained=1 and only eval 1003; eval 1001 must still be retained
	// because nonguix eval 50 references guixcommit1001!
	st.UpdateEvaluations([]EvaluationRecord{
		{ID: 1003, Commit: "guixcommit1003", Timestamp: ts.Add(time.Hour)},
	}, 1)

	if err := st.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	st2, err := Open(dbPath, "https://ci.guix.gnu.org", []string{"x86_64-linux"})
	if err != nil {
		t.Fatalf("Open reload failed: %v", err)
	}
	snap := st2.Snapshot()
	if _, ok := snap.GuixEvalByCommit["guixcommit1001"]; !ok {
		t.Fatalf("expected referenced guixcommit1001 to be retained in GuixEvalByCommit")
	}
	if nix, ok := st2.GetChannelBuildNixName("nonguix", 555); !ok || nix != "linux-6.12.1" {
		t.Fatalf("expected nonguix build 555 nixname linux-6.12.1, got %q", nix)
	}
	ngData := snap.ByChannelEvalSystem[ChannelEvalSystemKey{Channel: "nonguix", EvalID: 50, System: "x86_64-linux"}]
	if ngData == nil || len(ngData.JobsByBase["linux"]) != 2 {
		t.Fatalf("expected 2 linux jobs in nonguix eval 50, got %+v", ngData)
	}
	if ngData.JobsByBase["linux"][0].Version != "6.12.1" {
		t.Fatalf("expected version 6.12.1 on nonguix build 555, got %q", ngData.JobsByBase["linux"][0].Version)
	}
}
