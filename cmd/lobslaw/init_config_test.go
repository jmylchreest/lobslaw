package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

// renderInitConfig writes the template the way lobslaw init does.
func renderInitConfig(t *testing.T) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "config.toml")
	ans := initAnswers{
		Dir:              dir,
		ProviderLabel:    "openrouter",
		ProviderEndpoint: "https://openrouter.ai/api/v1/chat/completions",
		ProviderModel:    "anthropic/claude-sonnet-4",
	}
	err := writeConfigTOML(path, ans,
		filepath.Join(dir, "data"), filepath.Join(dir, "audit"),
		filepath.Join(dir, "certs", "ca.pem"),
		filepath.Join(dir, "certs", "node.pem"),
		filepath.Join(dir, "certs", "node-key.pem"),
		"OPENROUTER_API_KEY")
	if err != nil {
		t.Fatalf("writeConfigTOML: %v", err)
	}
	return dir, path
}

// The config `lobslaw init` generates has to load. It is the first
// thing a new operator runs and the last thing anyone re-reads, so
// template drift shows up as a broken first-run rather than as a
// failing test — unless there is a test.
func TestInitConfigLoads(t *testing.T) {
	t.Parallel()
	_, path := renderInitConfig(t)

	cfg, err := config.Load(config.LoadOptions{Path: path, SkipEnv: true})
	if err != nil {
		raw, _ := os.ReadFile(path)
		t.Fatalf("the generated config does not load: %v\n\n%s", err, raw)
	}
	if !cfg.Compute.Enabled || !cfg.Memory.Enabled {
		t.Error("the generated config should enable compute and memory")
	}
}

// disabled_tools is written out rather than left implicit, so an
// operator can see the posture they are running and change it. That
// only holds if it survives a round trip as an EXPLICIT value: nil
// would mean the template said nothing and the compiled-in default is
// doing the work invisibly, which is the thing this exists to stop.
func TestInitConfigStatesTheToolPostureExplicitly(t *testing.T) {
	t.Parallel()
	_, path := renderInitConfig(t)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `disabled_tools = ["remote_*"]`) {
		t.Error("the generated config should state disabled_tools, not inherit it silently")
	}

	cfg, err := config.Load(config.LoadOptions{Path: path, SkipEnv: true})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Compute.DisabledTools == nil {
		t.Fatal("disabled_tools round-tripped as nil; the operator's choice was lost")
	}
	if got := *cfg.Compute.DisabledTools; len(got) != 1 || got[0] != "remote_*" {
		t.Errorf("disabled_tools = %v, want [remote_*]", got)
	}
}

// The distinction the pointer exists for, exercised through the real
// loader rather than asserted in a comment.
func TestDisabledToolsDistinguishesAbsentFromEmpty(t *testing.T) {
	t.Parallel()
	base := `
[memory]
enabled = true
[memory.encryption]
key_ref = "env:LOBSLAW_MEMORY_KEY"
[memory.snapshot]
target = "storage:local-snapshots"
[storage]
enabled = true
[[storage.mounts]]
label = "local-snapshots"
type = "local"
path = "/tmp/lobslaw-test-snapshots"
[compute]
enabled = true
`
	cases := []struct {
		name  string
		extra string
		want  *[]string
	}{
		{"absent", "", nil},
		{"empty", "disabled_tools = []\n", &[]string{}},
		{"set", "disabled_tools = [\"remote_*\"]\n", &[]string{"remote_*"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(base+tc.extra), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(config.LoadOptions{Path: path, SkipEnv: true})
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			got := cfg.Compute.DisabledTools
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("an absent key should stay nil so the default applies, got %v", *got)
			case tc.want != nil && got == nil:
				t.Error("an explicit key round-tripped as nil; the default would silently win")
			case tc.want != nil && got != nil && len(*got) != len(*tc.want):
				t.Errorf("disabled_tools = %v, want %v", *got, *tc.want)
			}
		})
	}
}
