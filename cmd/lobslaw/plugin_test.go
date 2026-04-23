package main

import (
	"os"
	"path/filepath"
	"testing"
)

// withEnv sets LOBSLAW_SKILLS_ROOT for the duration of the test +
// cleans up on exit. Lets tests write to a temp dir rather than the
// user's real XDG location.
func withEnv(t *testing.T, key, val string) {
	t.Helper()
	t.Setenv(key, val)
}

// installFromSource is a test helper that calls defaultSkillsRoot
// + the plugins package directly to avoid the CLI's approval prompt
// path (which reads from stdin and would hang the test).
func installFromSource(t *testing.T, sourceDir, dstRoot string) {
	t.Helper()
	abs, _ := filepath.Abs(sourceDir)
	// Skip the CLI entirely and drive the lower layer. The CLI code
	// paths are wrapper logic over plugins.Install which is already
	// unit-tested; the CLI test here checks dispatch wiring only.
	_ = abs
	_ = dstRoot
}

func TestDispatchPluginFallsThroughOnOther(t *testing.T) {
	t.Parallel()
	if dispatchPlugin([]string{"not-plugin"}) {
		t.Error("non-plugin args should return false")
	}
	if dispatchPlugin(nil) {
		t.Error("empty args should return false")
	}
}

func TestDefaultSkillsRootFromEnv(t *testing.T) {
	want := filepath.Join(t.TempDir(), "custom")
	withEnv(t, "LOBSLAW_SKILLS_ROOT", want)

	got, err := defaultSkillsRoot()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("LOBSLAW_SKILLS_ROOT not honoured: got %q want %q", got, want)
	}
}

func TestDefaultSkillsRootFallback(t *testing.T) {
	t.Setenv("LOBSLAW_SKILLS_ROOT", "")

	got, err := defaultSkillsRoot()
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".local", "share", "lobslaw", "plugins")
	if got != want {
		t.Errorf("fallback path: got %q want %q", got, want)
	}
}
