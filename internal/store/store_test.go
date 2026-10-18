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
