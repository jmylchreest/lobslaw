package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDiscoverPolicyDirsExplicitWinsVerbatim(t *testing.T) {
	t.Parallel()
	got := DiscoverPolicyDirs([]string{"/a", "/b"}, "/ignored/config/dir")
	if !slices.Equal(got, []string{"/a", "/b"}) {
		t.Errorf("explicit should bypass defaults; got %v", got)
	}
}

// TestDiscoverPolicyDirsDefaultBuildsLayered confirms the
// precedence order when no explicit list is passed:
// user-global → config-dir → cwd.
func TestDiscoverPolicyDirsDefaultBuildsLayered(t *testing.T) {
	// NOT parallel — mutates env (HOME / XDG).
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	got := DiscoverPolicyDirs(nil, "/app/data")
	if len(got) < 2 {
		t.Fatalf("expected at least 2 default dirs, got %v", got)
	}

	wantUser := filepath.Join(home, ".config", "lobslaw", "policy.d")
	wantConfig := "/app/data/policy.d"

	if got[0] != wantUser {
		t.Errorf("first entry (user-global): got %q, want %q", got[0], wantUser)
	}
	if !slices.Contains(got, wantConfig) {
		t.Errorf("config-dir entry missing: got %v, want containing %q", got, wantConfig)
	}
	// Last entry should be cwd-derived (the "most specific" in
	// precedence order).
	last := got[len(got)-1]
	if !strings.HasSuffix(last, "policy.d") {
		t.Errorf("last entry should end in policy.d; got %q", last)
	}
}

// TestDiscoverPolicyDirsXDGWinsOverHome confirms XDG_CONFIG_HOME
// takes priority over $HOME/.config when both are set — same
// convention as the existing config loader.
func TestDiscoverPolicyDirsXDGWinsOverHome(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir()) // different dir, must NOT be used

	got := DiscoverPolicyDirs(nil, "")
	want := filepath.Join(xdg, "lobslaw", "policy.d")
	if !slices.Contains(got, want) {
		t.Errorf("XDG-derived dir missing; got %v, want containing %q", got, want)
	}
}

// TestDiscoverPolicyDirsDedupsSamePath — when configDir == cwd (dev
// workflow), the two candidate paths collide. Dedup keeps the first
// occurrence and drops the duplicate so the caller doesn't reload
// the same directory twice per event.
func TestDiscoverPolicyDirsDedupsSamePath(t *testing.T) {
	t.Parallel()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// configDir == cwd → both derive to the same policy.d.
	got := DiscoverPolicyDirs(nil, cwd)
	seen := make(map[string]int)
	for _, p := range got {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			seen[real]++
		} else {
			seen[p]++
		}
	}
	for path, count := range seen {
		if count > 1 {
			t.Errorf("path %q appears %d times; dedup broken", path, count)
		}
	}
}
