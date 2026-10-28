package cuirass

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSpecToNameAndVer(t *testing.T) {
	tests := []struct {
		spec    string
		wantPkg string
		wantVer string
	}{
		{"emacs", "emacs", ""},
		{"gcc-toolchain@14", "gcc-toolchain", "14"},
		{"bind:utils", "bind", ""},
		{"gcc-toolchain@13.3:debug", "gcc-toolchain", "13.3"},
		{"  git  ", "git", ""},
	}
	for _, tc := range tests {
		pkg, ver := SpecToNameAndVer(tc.spec)
		if pkg != tc.wantPkg || ver != tc.wantVer {
			t.Errorf("SpecToNameAndVer(%q) = (%q, %q), want (%q, %q)",
				tc.spec, pkg, ver, tc.wantPkg, tc.wantVer)
		}
	}
}

func TestExtractNixnameAndVersion(t *testing.T) {
	drv := "/gnu/store/29zj3k2l19a8s7d6f5g4h3j2k1l0m9n8-gcc-toolchain-14.2.0.drv"
	nix := ExtractNixname(drv)
	if nix != "gcc-toolchain-14.2.0" {
		t.Fatalf("ExtractNixname(%q) = %q, want gcc-toolchain-14.2.0", drv, nix)
	}
	ver := NixnameToVersion("gcc-toolchain", nix)
	if ver != "14.2.0" {
		t.Fatalf("NixnameToVersion = %q, want 14.2.0", ver)
	}
}

func TestVersionGreater(t *testing.T) {
	tests := []struct {
		v1   string
		v2   string
		want bool
	}{
		{"14.2.0", "13.3.0", true},
		{"13.3.0", "14.2.0", false},
		{"6.12.5", "6.6.65", true},
		{"29.4", "29.4", false},
		{"1.10.0", "1.2.0", true},
		{"2.0.1", "2.0.0", true},
	}
	for _, tc := range tests {
		if got := VersionGreater(tc.v1, tc.v2); got != tc.want {
			t.Errorf("VersionGreater(%q, %q) = %v, want %v", tc.v1, tc.v2, got, tc.want)
		}
	}
}

func TestClientRetryOn504(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":101,"specification":"master","timestamp":1729170000,"checkouts":[{"commit":"deadbeef12345678","channel":"guix"}]}]`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, WithRetry(3, 5*time.Millisecond))
	evals, err := client.LatestEvaluations(context.Background(), "master", 10)
	if err != nil {
		t.Fatalf("LatestEvaluations failed after retries: %v", err)
	}
	if len(evals) != 1 || evals[0].ID != 101 || evals[0].PrimaryCommit() != "deadbeef12345678" {
		t.Fatalf("unexpected evaluations response: %+v", evals)
	}
	if attempts.Load() != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts.Load())
	}
}

func TestBuildFinishedAndStatus(t *testing.T) {
	bFinished := Build{
		ID:          26715933,
		NixName:     "gr-iqbal-0.38.3",
		Derivation:  "/gnu/store/nlkvg0xk2c2lzxs97nd5x2l8s6zqjwaz-gr-iqbal-0.38.3.drv",
		Finished:    1,
		BuildStatus: 0,
	}
	if !bFinished.IsFinished() {
		t.Errorf("expected bFinished.IsFinished() == true")
	}
	if bFinished.Status() != StatusSucceeded {
		t.Errorf("expected StatusSucceeded, got %v", bFinished.Status())
	}

	bFailed := Build{
		ID:          12345,
		Finished:    1,
		BuildStatus: 1,
	}
	if !bFailed.IsFinished() {
		t.Errorf("expected bFailed.IsFinished() == true")
	}
	if bFailed.Status() != StatusFailed {
		t.Errorf("expected StatusFailed, got %v", bFailed.Status())
	}

	bBuilding := Build{
		ID:          54321,
		Finished:    0,
		BuildStatus: -1,
	}
	if bBuilding.IsFinished() {
		t.Errorf("expected bBuilding.IsFinished() == false")
	}
	if bBuilding.Status() != StatusStarted {
		t.Errorf("expected StatusStarted, got %v", bBuilding.Status())
	}
}
