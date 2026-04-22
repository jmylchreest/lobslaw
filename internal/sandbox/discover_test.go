package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverPolicyDirExplicitWins(t *testing.T) {
	t.Parallel()
	got := DiscoverPolicyDir("/custom/path", "/ignored/config/dir")
	if got != "/custom/path" {
		t.Errorf("explicit should win; got %q", got)
	}
}

func TestDiscoverPolicyDirDerivesFromConfigDir(t *testing.T) {
	t.Parallel()
	got := DiscoverPolicyDir("", "/app/data")
	want := filepath.Join("/app/data", "policy.d")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestDiscoverPolicyDirFallsBackToCWD — the "env-only" path: no
// config.toml was found, so configDir is empty. We should return a
// CWD-relative policy.d so local-dev and containers without config
// files still have a sensible default.
func TestDiscoverPolicyDirFallsBackToCWD(t *testing.T) {
	t.Parallel()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got := DiscoverPolicyDir("", "")
	wantPrefix := cwd
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("got %q, want it to be under CWD %q", got, wantPrefix)
	}
	if filepath.Base(got) != "policy.d" {
		t.Errorf("got %q, want path ending in policy.d", got)
	}
}
