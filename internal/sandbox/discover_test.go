package sandbox

import (
	"os"
	"path/filepath"
	"slices"
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
	// Config-dir is last, and therefore highest precedence: it is the
	// most specific location an operator actually chose.
	if last := got[len(got)-1]; last != wantConfig {
		t.Errorf("last entry (highest precedence): got %q, want %q", last, wantConfig)
	}
}

// The working directory is NOT searched. It was, and dropping it is
// the point: a policy file can loosen a tool's sandbox as well as
// tighten it, so discovering one because of where the binary was
// started makes a shell prompt into a privilege decision.
func TestDiscoverPolicyDirsIgnoresWorkingDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got := DiscoverPolicyDirs(nil, "/app/data")
	if slices.Contains(got, filepath.Join(cwd, "policy.d")) {
		t.Errorf("the working directory was searched: %v", got)
	}
	for _, p := range got {
		if p == "./policy.d" {
			t.Errorf("relative cwd fallback present: %v", got)
		}
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

// Dedup keeps the first occurrence so the caller does not load the
// same directory twice per event. Exercised with configDir pointing at
// the user-global location, which is the collision that remains now
// that the working directory is no longer searched.
func TestDiscoverPolicyDirsDedupsSamePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	got := DiscoverPolicyDirs(nil, filepath.Join(home, ".config", "lobslaw"))
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
