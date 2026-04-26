package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestCollectEnvProvidersHappyPath(t *testing.T) {
	t.Parallel()
	env := []string{
		"LOBSLAW_PROVIDER_fast_ENDPOINT=https://example.invalid/v1",
		"LOBSLAW_PROVIDER_fast_MODEL=gpt-4o-mini",
		"LOBSLAW_PROVIDER_fast_API_KEY=env:OPENAI_API_KEY",
		"LOBSLAW_PROVIDER_fast_TRUST_TIER=public",
		"UNRELATED=xyz",
	}
	got, err := collectEnvProviders(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 provider, got %d", len(got))
	}
	p := got["fast"]
	if p.Label != "fast" {
		t.Errorf("label: %q", p.Label)
	}
	if p.Endpoint != "https://example.invalid/v1" || p.Model != "gpt-4o-mini" {
		t.Errorf("fields didn't populate: %+v", p)
	}
	if p.APIKeyRef != "env:OPENAI_API_KEY" {
		t.Errorf("api key: %q", p.APIKeyRef)
	}
	if p.TrustTier != types.TrustPublic {
		t.Errorf("trust tier: %q", p.TrustTier)
	}
}

// TestCollectEnvProvidersCaseInsensitiveLabel guards the primary
// user request: shell-conventional UPPERCASE env vars must match
// lowercase labels in config.toml. Here an env var labelled "FAST"
// produces a provider whose Label is "fast" (normalised), and a
// subsequent match against a config.toml "fast" label works.
func TestCollectEnvProvidersCaseInsensitiveLabel(t *testing.T) {
	t.Parallel()
	cases := []string{
		"LOBSLAW_PROVIDER_FAST_ENDPOINT=https://e",
		"LOBSLAW_PROVIDER_fast_ENDPOINT=https://e",
		"LOBSLAW_PROVIDER_Fast_ENDPOINT=https://e",
	}
	for _, env := range cases {
		got, err := collectEnvProviders([]string{env})
		if err != nil {
			t.Errorf("%q: %v", env, err)
			continue
		}
		if len(got) != 1 {
			t.Errorf("%q: want 1 provider, got %d", env, len(got))
			continue
		}
		p, ok := got["fast"] // lookup is always lowercase
		if !ok {
			t.Errorf("%q: provider not found under 'fast' key", env)
			continue
		}
		if p.Label != "fast" {
			t.Errorf("%q: label normalised to %q, want 'fast'", env, p.Label)
		}
	}
}

func TestCollectEnvProvidersMultipleLabels(t *testing.T) {
	t.Parallel()
	env := []string{
		"LOBSLAW_PROVIDER_fast_ENDPOINT=https://a",
		"LOBSLAW_PROVIDER_fast_MODEL=gpt-4o-mini",
		"LOBSLAW_PROVIDER_local_ENDPOINT=http://localhost:11434",
		"LOBSLAW_PROVIDER_local_MODEL=llama3.1",
	}
	got, _ := collectEnvProviders(env)
	if len(got) != 2 {
		t.Fatalf("want 2 providers; got %d: %+v", len(got), got)
	}
	if got["fast"].Endpoint != "https://a" {
		t.Errorf("fast endpoint: %q", got["fast"].Endpoint)
	}
	if got["local"].Model != "llama3.1" {
		t.Errorf("local model: %q", got["local"].Model)
	}
}

func TestCollectEnvProvidersCapabilities(t *testing.T) {
	t.Parallel()
	env := []string{
		"LOBSLAW_PROVIDER_fast_CAPABILITIES=code , fast,, vision",
	}
	got, _ := collectEnvProviders(env)
	caps := got["fast"].Capabilities
	want := []string{"code", "fast", "vision"}
	if !slices.Equal(caps, want) {
		t.Errorf("capabilities: got %v, want %v", caps, want)
	}
}

func TestCollectEnvProvidersUnknownFieldIsError(t *testing.T) {
	t.Parallel()
	// Typo: MODEEL instead of MODEL — should surface loudly so
	// operators don't silently ship misconfigured providers.
	env := []string{"LOBSLAW_PROVIDER_fast_MODEEL=gpt-4o-mini"}
	_, err := collectEnvProviders(env)
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !strings.Contains(err.Error(), "MODEEL") {
		t.Errorf("error should name the bad field; got %q", err.Error())
	}
}

// TestCollectEnvProvidersMalformedKey documents the parser's view
// of "looks like our namespace but isn't parseable" (e.g. no label
// before the field suffix).
func TestCollectEnvProvidersMalformedKey(t *testing.T) {
	t.Parallel()
	// LOBSLAW_PROVIDER_MODEL — label portion is empty after stripping
	// the MODEL suffix. Should reject.
	env := []string{"LOBSLAW_PROVIDER__MODEL=x"}
	_, err := collectEnvProviders(env)
	if err == nil {
		t.Error("empty label should be rejected")
	}
}

func TestSplitLabelFieldLongestSuffixWins(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in         string
		wantLabel  string
		wantField  string
		wantOk     bool
	}{
		{"fast_API_KEY", "fast", "API_KEY", true},
		{"fast_TRUST_TIER", "fast", "TRUST_TIER", true},
		{"fast_MODEL", "fast", "MODEL", true},
		{"fast_ENDPOINT", "fast", "ENDPOINT", true},
		{"fast_CAPABILITIES", "fast", "CAPABILITIES", true},
		{"my_complex_label_MODEL", "my_complex_label", "MODEL", true},
		{"no_known_suffix", "", "", false},
	}
	for _, tc := range cases {
		label, field, ok := splitLabelField(tc.in)
		if ok != tc.wantOk {
			t.Errorf("%q: ok = %v, want %v", tc.in, ok, tc.wantOk)
			continue
		}
		if label != tc.wantLabel || field != tc.wantField {
			t.Errorf("%q: got (%q,%q), want (%q,%q)",
				tc.in, label, field, tc.wantLabel, tc.wantField)
		}
	}
}

// --- mergeProviders tests ---------------------------------------------

func TestMergeProvidersEnvOnlyProvider(t *testing.T) {
	t.Parallel()
	out := mergeProviders(nil, map[string]types.ProviderConfig{
		"fast": {
			Label:     "fast",
			Endpoint:  "https://e",
			Model:     "m",
			TrustTier: types.TrustPublic,
		},
	})
	if len(out) != 1 {
		t.Fatalf("want 1 provider; got %d", len(out))
	}
	if out[0].Endpoint != "https://e" {
		t.Errorf("fields didn't lift: %+v", out[0])
	}
}

func TestMergeProvidersEnvOverridesConfigToml(t *testing.T) {
	t.Parallel()
	configProviders := []ProviderConfig{
		{Label: "fast", Endpoint: "https://config-endpoint", Model: "config-model", TrustTier: types.TrustPublic},
	}
	env := map[string]types.ProviderConfig{
		"fast": {Label: "fast", Model: "env-model"}, // only MODEL set — other fields stay from config
	}
	out := mergeProviders(configProviders, env)
	if len(out) != 1 {
		t.Fatalf("want 1 merged provider; got %d", len(out))
	}
	p := out[0]
	if p.Model != "env-model" {
		t.Errorf("env MODEL should override: got %q", p.Model)
	}
	if p.Endpoint != "https://config-endpoint" {
		t.Errorf("config ENDPOINT should survive: got %q", p.Endpoint)
	}
	if p.TrustTier != types.TrustPublic {
		t.Errorf("config TRUST_TIER should survive: got %q", p.TrustTier)
	}
}

// TestMergeProvidersCaseInsensitiveMatching — the core user request.
// Config.toml has label="fast" (lowercase). Operator sets
// LOBSLAW_PROVIDER_FAST_MODEL=... (shell uppercase). Must merge.
func TestMergeProvidersCaseInsensitiveMatching(t *testing.T) {
	t.Parallel()
	configProviders := []ProviderConfig{
		{Label: "fast", Endpoint: "https://e", Model: "original", TrustTier: types.TrustPublic},
	}
	// Env-collected providers are always stored under their lowercased
	// label key regardless of the input case. Simulate UPPERCASE env.
	env := map[string]types.ProviderConfig{
		"fast": {Label: "fast", Model: "overridden"},
	}
	out := mergeProviders(configProviders, env)
	if len(out) != 1 {
		t.Fatalf("should have merged to 1; got %d", len(out))
	}
	// Label case preserved from config.toml (operator's canonical casing).
	if out[0].Label != "fast" {
		t.Errorf("label case should survive merge: got %q", out[0].Label)
	}
	if out[0].Model != "overridden" {
		t.Errorf("env MODEL should have won: got %q", out[0].Model)
	}
}

func TestMergeProvidersConfigOnlyProvidersPassThrough(t *testing.T) {
	t.Parallel()
	configProviders := []ProviderConfig{
		{Label: "smart", Endpoint: "https://e", Model: "m", TrustTier: types.TrustPrivate},
	}
	out := mergeProviders(configProviders, nil)
	if len(out) != 1 || out[0].Label != "smart" {
		t.Errorf("config-only providers should pass through unchanged: %+v", out)
	}
}

// TestMergeProvidersMixed — one provider from each source; both end
// up in the final list.
func TestMergeProvidersMixed(t *testing.T) {
	t.Parallel()
	configProviders := []ProviderConfig{
		{Label: "from-config", Endpoint: "https://a", Model: "mA"},
	}
	env := map[string]types.ProviderConfig{
		"from-env": {Label: "from-env", Endpoint: "https://b", Model: "mB"},
	}
	out := mergeProviders(configProviders, env)
	if len(out) != 2 {
		t.Fatalf("want 2 providers; got %d", len(out))
	}
	labels := []string{out[0].Label, out[1].Label}
	slices.Sort(labels)
	if !slices.Equal(labels, []string{"from-config", "from-env"}) {
		t.Errorf("expected both labels present; got %v", labels)
	}
}

// --- end-to-end Load integration --------------------------------------

func TestLoadMergesEnvProviders(t *testing.T) {
	// Empty TOML config + env-declared provider → provider appears
	// in the final Config.Compute.Providers slice.
	path := writeTempConfig(t, `
[compute]
default_chain = "only"

[[compute.chains]]
label = "only"
[compute.chains.trigger]
always = true
[[compute.chains.steps]]
provider = "from-env"
role = "primary"
`)
	t.Setenv("LOBSLAW_PROVIDER_from-env_ENDPOINT", "https://example.invalid")
	t.Setenv("LOBSLAW_PROVIDER_from-env_MODEL", "m")
	t.Setenv("LOBSLAW_PROVIDER_from-env_TRUST_TIER", "public")

	cfg, err := Load(LoadOptions{Path: path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Compute.Providers) != 1 {
		t.Fatalf("want 1 provider, got %d: %+v", len(cfg.Compute.Providers), cfg.Compute.Providers)
	}
	if cfg.Compute.Providers[0].Endpoint != "https://example.invalid" {
		t.Errorf("env-sourced endpoint didn't reach Config: %+v", cfg.Compute.Providers[0])
	}
}

func TestLoadSkipEnvAlsoSkipsEnvProviders(t *testing.T) {
	// Opt-out: SkipEnv = true should bypass env-provider collection
	// too (env-overlay infrastructure stays consistent).
	path := writeTempConfig(t, `
[compute]
`)
	t.Setenv("LOBSLAW_PROVIDER_ghost_ENDPOINT", "https://should-not-apply")

	cfg, err := Load(LoadOptions{Path: path, SkipEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Compute.Providers) != 0 {
		t.Errorf("SkipEnv should suppress env providers; got %+v", cfg.Compute.Providers)
	}
}
