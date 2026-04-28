package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

const miniConfig = `
[memory]
enabled = true

[storage]
enabled = true

[memory.encryption]
key_ref = "env:LOBSLAW_TEST_MEMORY_KEY"

[memory.snapshot]
target = "storage:r2-backup"
cadence = "1h"

[[compute.providers]]
label = "fast"
endpoint = "https://api.openrouter.ai/api/v1"
model = "meta/llama-3.1-8b"
api_key_ref = "env:OPENROUTER_API_KEY_LOBSLAW"
trust_tier = "public"

[compute.budgets]
max_tool_calls_per_turn = 30
max_spend_usd_per_turn = 0.50
max_egress_bytes_per_turn = 10000000

[audit.local]
enabled = true
path = "/var/lobslaw/audit/audit.jsonl"
max_size_mb = 100
max_files = 10
`

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoadFromExplicitPath(t *testing.T) {
	t.Parallel()
	path := writeTempConfig(t, miniConfig)

	cfg, err := Load(LoadOptions{Path: path, SkipEnv: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Path / Dir are the hook for downstream code (sandbox policy_dir
	// resolution) to locate sibling files without introducing a
	// parallel env-var chain.
	if cfg.Path() != path {
		t.Errorf("Path() = %q, want %q", cfg.Path(), path)
	}
	if cfg.Dir() == "" {
		t.Error("Dir() should be non-empty when Path() is set")
	}

	if !cfg.Memory.Enabled {
		t.Error("Memory.Enabled should be true")
	}
	if cfg.Memory.Snapshot.Cadence != time.Hour {
		t.Errorf("Snapshot.Cadence = %v, want 1h", cfg.Memory.Snapshot.Cadence)
	}
	if len(cfg.Compute.Providers) != 1 {
		t.Fatalf("want 1 provider, got %d", len(cfg.Compute.Providers))
	}
	if cfg.Compute.Providers[0].TrustTier != types.TrustPublic {
		t.Errorf("Provider[0].TrustTier = %q, want public", cfg.Compute.Providers[0].TrustTier)
	}
	if cfg.Compute.Budgets.MaxToolCallsPerTurn != 30 {
		t.Errorf("Budgets.MaxToolCallsPerTurn = %d, want 30", cfg.Compute.Budgets.MaxToolCallsPerTurn)
	}
	if !cfg.Audit.Local.Enabled {
		t.Error("Audit.Local.Enabled should be true")
	}
}

func TestLoadEnvOverride(t *testing.T) {
	path := writeTempConfig(t, miniConfig)

	t.Setenv("LOBSLAW__MEMORY__ENCRYPTION__KEY_REF", "env:OVERRIDDEN_KEY")

	cfg, err := Load(LoadOptions{Path: path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Memory.Encryption.KeyRef != "env:OVERRIDDEN_KEY" {
		t.Errorf("Memory.Encryption.KeyRef = %q, want env:OVERRIDDEN_KEY", cfg.Memory.Encryption.KeyRef)
	}
}

func TestLoadMissingExplicitPath(t *testing.T) {
	t.Parallel()
	_, err := Load(LoadOptions{Path: "/nonexistent/path/config.toml", SkipEnv: true})
	if err == nil {
		t.Fatal("expected error for missing explicit path")
	}
	if !errors.Is(err, types.ErrInvalidConfig) {
		t.Errorf("error = %v, want wraps ErrInvalidConfig", err)
	}
}

func TestLoadMissingMemoryKeyRef(t *testing.T) {
	t.Parallel()
	const cfgText = `
[memory]
enabled = true

[storage]
enabled = true
# memory.encryption.key_ref intentionally missing
`
	path := writeTempConfig(t, cfgText)
	_, err := Load(LoadOptions{Path: path, SkipEnv: true})
	if err == nil {
		t.Fatal("expected error when memory is enabled without key_ref")
	}
	if !errors.Is(err, types.ErrInvalidConfig) {
		t.Errorf("err = %v, want wraps ErrInvalidConfig", err)
	}
}

func TestLoadMemoryDisabledNoKeyRefOK(t *testing.T) {
	t.Parallel()
	const cfgText = `
[memory]
enabled = false
`
	path := writeTempConfig(t, cfgText)
	if _, err := Load(LoadOptions{Path: path, SkipEnv: true}); err != nil {
		t.Errorf("memory disabled should not require key_ref, got %v", err)
	}
}

func TestLoadNoFileEnvOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("LOBSLAW__MEMORY__ENCRYPTION__KEY_REF", "env:ENV_ONLY_KEY")

	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load without config file should succeed: %v", err)
	}
	if cfg.Memory.Encryption.KeyRef != "env:ENV_ONLY_KEY" {
		t.Errorf("Memory.Encryption.KeyRef = %q, want env:ENV_ONLY_KEY (env-only load failed)", cfg.Memory.Encryption.KeyRef)
	}
	// Path()/Dir() should be empty when Load ran env-only — downstream
	// discovery code uses this as the "fall back to CWD" signal.
	if cfg.Path() != "" {
		t.Errorf("env-only Load: Path() should be empty, got %q", cfg.Path())
	}
	if cfg.Dir() != "" {
		t.Errorf("env-only Load: Dir() should be empty, got %q", cfg.Dir())
	}
}

// TestLoadDoesNotFallBackToEtc guards the container-first posture:
// /etc/lobslaw/config.toml was removed from the fallback chain per
// the deployment model (containers use CWD or mounted volumes; dev
// uses CWD or $XDG_CONFIG_HOME). A config at /etc is no longer
// findable by default — ensures we don't regress if someone adds it
// back.
func TestLoadDoesNotFallBackToEtc(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	// Point $HOME / $XDG_CONFIG_HOME somewhere without a config so
	// only a hypothetical /etc/lobslaw/config.toml could match.
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load should succeed with no findable config: %v", err)
	}
	if cfg.Path() != "" {
		t.Errorf("/etc/lobslaw should NOT be in the fallback chain; resolved to %q", cfg.Path())
	}
}
