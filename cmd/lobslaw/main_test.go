package main

import (
	"reflect"
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
	if err := parseFlags([]string{"--all", "--config", "/etc/lobslaw/config.toml"}, &f); err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !f.all {
		t.Error("--all not set")
	}
	if f.configPath != "/etc/lobslaw/config.toml" {
		t.Errorf("configPath = %q", f.configPath)
	}
}
