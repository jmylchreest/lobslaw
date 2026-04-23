package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestResolveFunctionsAll(t *testing.T) {
	t.Parallel()
	got := resolveFunctions(flags{all: true}, &config.Config{})
	want := allFunctions()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("--all → %v, want %v", got, want)
	}
}

func TestResolveFunctionsExplicit(t *testing.T) {
	t.Parallel()
	got := resolveFunctions(flags{memory: true, gateway: true}, &config.Config{})
	want := []types.NodeFunction{types.FunctionMemory, types.FunctionGateway}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("--memory --gateway → %v, want %v", got, want)
	}
}

func TestResolveFunctionsFromConfig(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Memory:  config.MemoryConfig{Enabled: true},
		Compute: config.ComputeConfig{Enabled: true},
	}
	got := resolveFunctions(flags{}, cfg)
	want := []types.NodeFunction{types.FunctionMemory, types.FunctionCompute}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("no flags + config memory+compute → %v, want %v", got, want)
	}
}

func TestResolveFunctionsDefault(t *testing.T) {
	t.Parallel()
	got := resolveFunctions(flags{}, &config.Config{})
	want := allFunctions()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("nothing specified → %v, want %v (all)", got, want)
	}
}

func TestResolveFunctionsExplicitOverridesConfig(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Memory:  config.MemoryConfig{Enabled: true},
		Policy:  config.PolicyConfig{Enabled: true},
		Compute: config.ComputeConfig{Enabled: true},
		Gateway: config.GatewayConfig{Enabled: true},
	}
	got := resolveFunctions(flags{gateway: true}, cfg)
	want := []types.NodeFunction{types.FunctionGateway}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("--gateway + config-all-enabled → %v, want %v (flag wins)", got, want)
	}
}

func TestParseFlagsHelp(t *testing.T) {
	t.Parallel()
	var f flags
	err := parseFlags([]string{"--help"}, &f)
	if err == nil {
		t.Error("--help should return flag.ErrHelp")
	}
}

func TestParseFlagsAll(t *testing.T) {
	t.Parallel()
	var f flags
	if err := parseFlags([]string{"--all", "--config", "/app/data/config.toml"}, &f); err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !f.all {
		t.Error("--all not set")
	}
	if f.configPath != "/app/data/config.toml" {
		t.Errorf("configPath = %q", f.configPath)
	}
}

// TestParseFlagsPolicyDirRepeatable — operators can pass --policy-dir
// multiple times to layer directories. Later entries override
// earlier ones per last-write-wins, so the parser must preserve
// order (no set-based dedup here; discover.go's dedup runs later).
func TestParseFlagsPolicyDirRepeatable(t *testing.T) {
	t.Parallel()
	var f flags
	args := []string{
		"--policy-dir", "/corp/defaults",
		"--policy-dir", "/home/alice/.config/lobslaw/policy.d",
		"--policy-dir", "/srv/project/policy.d",
	}
	if err := parseFlags(args, &f); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/corp/defaults",
		"/home/alice/.config/lobslaw/policy.d",
		"/srv/project/policy.d",
	}
	if len(f.policyDirs) != len(want) {
		t.Fatalf("policyDirs len = %d, want %d: got %v", len(f.policyDirs), len(want), f.policyDirs)
	}
	for i, got := range f.policyDirs {
		if got != want[i] {
			t.Errorf("policyDirs[%d] = %q, want %q", i, got, want[i])
		}
	}
}

func TestParseFlagsPolicyDirEmpty(t *testing.T) {
	t.Parallel()
	var f flags
	if err := parseFlags([]string{}, &f); err != nil {
		t.Fatal(err)
	}
	if f.policyDirs != nil {
		t.Errorf("no flags → no policyDirs; got %v", f.policyDirs)
	}
}

// --- resolvePolicyDirs precedence tests ---------------------------------

func TestResolvePolicyDirsCLIWins(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Sandbox: config.SandboxConfig{PolicyDirs: []string{"/from-config"}},
	}
	got := resolvePolicyDirs([]string{"/from-cli"}, cfg)
	if len(got) != 1 || got[0] != "/from-cli" {
		t.Errorf("CLI should win over config; got %v", got)
	}
}

func TestResolvePolicyDirsConfigWinsOverDiscovery(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Sandbox: config.SandboxConfig{PolicyDirs: []string{"/from-config"}},
	}
	got := resolvePolicyDirs(nil, cfg)
	if len(got) != 1 || got[0] != "/from-config" {
		t.Errorf("config-set PolicyDirs should skip default discovery; got %v", got)
	}
}

// TestResolvePolicyDirsEmptyFallsBackToDiscovery — the common case:
// operator configures nothing, we derive the list from HOME/XDG +
// configDir + CWD. Only assert "non-empty + list contains a
// policy.d suffix somewhere" to avoid coupling to the test env's
// actual HOME path.
func TestResolvePolicyDirsEmptyFallsBackToDiscovery(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	got := resolvePolicyDirs(nil, cfg)
	if len(got) == 0 {
		t.Error("default discovery should yield at least one candidate")
	}
	for _, p := range got {
		if !strings.HasSuffix(p, "policy.d") {
			t.Errorf("default-discovered path should end in policy.d; got %q", p)
		}
	}
}

func TestPolicyDirsSourceLabels(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	if got := policyDirsSource([]string{"/x"}, cfg); got != "cli" {
		t.Errorf("cli source label: got %q", got)
	}
	cfg2 := &config.Config{Sandbox: config.SandboxConfig{PolicyDirs: []string{"/x"}}}
	if got := policyDirsSource(nil, cfg2); got != "config" {
		t.Errorf("config source label: got %q", got)
	}
	if got := policyDirsSource(nil, &config.Config{}); got != "default-discovery" {
		t.Errorf("default source label: got %q", got)
	}
}
