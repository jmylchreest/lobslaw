package compute

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// providerAt builds a minimal ProviderConfig for tests.
func providerAt(label string, tier types.TrustTier) config.ProviderConfig {
	return config.ProviderConfig{
		Label:     label,
		Endpoint:  "https://example.invalid/v1",
		Model:     label + "-model",
		TrustTier: tier,
	}
}

func TestNewResolverValidConfig(t *testing.T) {
	t.Parallel()
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{
			providerAt("ollama", types.TrustLocal),
			providerAt("openrouter", types.TrustPublic),
		},
		Chains: []config.ChainConfig{
			{
				Label:   "everyday",
				Steps:   []config.ChainStepConfig{{Provider: "openrouter", Role: "primary"}},
				Trigger: config.ChainTriggerConfig{Always: true},
			},
		},
		DefaultChain: "everyday",
	}
	r, err := NewResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r == nil {
		t.Fatal("nil resolver")
	}
}

func TestNewResolverReportsAllProblems(t *testing.T) {
	t.Parallel()
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{
			{Label: "", TrustTier: types.TrustPublic}, // empty label
			providerAt("dup", types.TrustPublic),
			providerAt("dup", types.TrustPublic),  // duplicate
			{Label: "bad-tier", TrustTier: "wtf"}, // bad tier
		},
		Chains: []config.ChainConfig{
			{Label: "", Steps: []config.ChainStepConfig{{Provider: "dup"}}}, // empty chain label
			{Label: "empty-steps", Steps: nil},                              // no steps
			{
				Label: "unknown-provider",
				Steps: []config.ChainStepConfig{{Provider: "does-not-exist"}},
			},
		},
		DefaultChain: "no-such-chain",
	}
	_, err := NewResolver(cfg)
	if err == nil {
		t.Fatal("expected aggregate error; got nil")
	}
	msg := err.Error()
	// Spot-check: the loader should report ALL classes of problem, not
	// bail on the first one.
	for _, wantSubstr := range []string{
		"empty label",
		"duplicate provider",
		"invalid trust_tier",
		"empty label", // chain empty label (same text; appears twice)
		"no steps",
		"unknown provider",
		"default_chain",
	} {
		if !strings.Contains(msg, wantSubstr) {
			t.Errorf("expected error to mention %q; got:\n%s", wantSubstr, msg)
		}
	}
}

func TestResolveAlwaysTriggerMatches(t *testing.T) {
	t.Parallel()
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{providerAt("p", types.TrustPublic)},
		Chains: []config.ChainConfig{
			{
				Label:   "always",
				Steps:   []config.ChainStepConfig{{Provider: "p"}},
				Trigger: config.ChainTriggerConfig{Always: true},
			},
		},
	}
	r, err := NewResolver(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := r.Resolve(ResolveRequest{Complexity: 50, MinTrustTier: types.TrustPublic})
	if err != nil {
		t.Fatal(err)
	}
	if dec.ChainLabel != "always" {
		t.Errorf("want chain 'always', got %q", dec.ChainLabel)
	}
	if len(dec.Steps) != 1 || dec.Steps[0].Provider.Label != "p" {
		t.Errorf("wrong steps: %+v", dec.Steps)
	}
	if dec.Steps[0].Role != "primary" {
		t.Errorf("empty Role should default to 'primary'; got %q", dec.Steps[0].Role)
	}
}

func TestResolveComplexityTriggerBelowFloorDoesNotMatch(t *testing.T) {
	t.Parallel()
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{
			providerAt("quick", types.TrustPublic),
			providerAt("smart", types.TrustPublic),
		},
		Chains: []config.ChainConfig{
			{
				Label:   "smart-chain",
				Steps:   []config.ChainStepConfig{{Provider: "smart"}},
				Trigger: config.ChainTriggerConfig{MinComplexity: 70},
			},
		},
		DefaultChain: "",
	}
	r, err := NewResolver(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Below trigger → should fall through to the synthesised single-
	// step pick.
	dec, err := r.Resolve(ResolveRequest{Complexity: 50, MinTrustTier: types.TrustPublic})
	if err != nil {
		t.Fatal(err)
	}
	if dec.ChainLabel == "smart-chain" {
		t.Error("smart-chain shouldn't have fired under trigger floor")
	}
}

func TestResolveComplexityTriggerAtOrAboveFloorMatches(t *testing.T) {
	t.Parallel()
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{providerAt("smart", types.TrustPublic)},
		Chains: []config.ChainConfig{
			{
				Label:   "smart-chain",
				Steps:   []config.ChainStepConfig{{Provider: "smart"}},
				Trigger: config.ChainTriggerConfig{MinComplexity: 70},
			},
		},
	}
	r, _ := NewResolver(cfg)
	dec, err := r.Resolve(ResolveRequest{Complexity: 70, MinTrustTier: types.TrustPublic})
	if err != nil {
		t.Fatal(err)
	}
	if dec.ChainLabel != "smart-chain" {
		t.Errorf("at-floor complexity should match; got chain %q", dec.ChainLabel)
	}
}

func TestResolveDomainTriggerAnyOverlap(t *testing.T) {
	t.Parallel()
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{providerAt("finance-llm", types.TrustPrivate)},
		Chains: []config.ChainConfig{
			{
				Label:   "finance",
				Steps:   []config.ChainStepConfig{{Provider: "finance-llm"}},
				Trigger: config.ChainTriggerConfig{Domains: []string{"finance", "legal"}},
			},
		},
	}
	r, _ := NewResolver(cfg)

	dec, err := r.Resolve(ResolveRequest{
		Domains:      []string{"health", "finance"},
		MinTrustTier: types.TrustPublic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if dec.ChainLabel != "finance" {
		t.Errorf("expected 'finance' chain on domain overlap; got %q", dec.ChainLabel)
	}
}

func TestResolveDomainTriggerNoOverlapDoesNotMatch(t *testing.T) {
	t.Parallel()
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{providerAt("fallback", types.TrustPublic)},
		Chains: []config.ChainConfig{
			{
				Label:   "finance",
				Steps:   []config.ChainStepConfig{{Provider: "fallback"}},
				Trigger: config.ChainTriggerConfig{Domains: []string{"finance"}},
			},
		},
	}
	r, _ := NewResolver(cfg)
	dec, err := r.Resolve(ResolveRequest{
		Domains:      []string{"health"},
		MinTrustTier: types.TrustPublic,
	})
	if err != nil {
		t.Fatal(err)
	}
	// No chain matched, no default → synthesised single-step pick.
	if dec.ChainLabel != "" {
		t.Errorf("no match should produce synthesised (empty-label) decision; got %q", dec.ChainLabel)
	}
}

func TestResolveFirstMatchingChainWins(t *testing.T) {
	t.Parallel()
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{
			providerAt("a", types.TrustPublic),
			providerAt("b", types.TrustPublic),
		},
		Chains: []config.ChainConfig{
			{
				Label:   "first",
				Steps:   []config.ChainStepConfig{{Provider: "a"}},
				Trigger: config.ChainTriggerConfig{Always: true},
			},
			{
				Label:   "second",
				Steps:   []config.ChainStepConfig{{Provider: "b"}},
				Trigger: config.ChainTriggerConfig{Always: true},
			},
		},
	}
	r, _ := NewResolver(cfg)
	dec, _ := r.Resolve(ResolveRequest{MinTrustTier: types.TrustPublic})
	if dec.ChainLabel != "first" {
		t.Errorf("first chain should win; got %q", dec.ChainLabel)
	}
}

func TestResolveDefaultChainFallback(t *testing.T) {
	t.Parallel()
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{providerAt("default-p", types.TrustPublic)},
		Chains: []config.ChainConfig{
			{
				Label: "default-chain",
				Steps: []config.ChainStepConfig{{Provider: "default-p"}},
				// No trigger set — this chain can ONLY fire as default.
				Trigger: config.ChainTriggerConfig{},
			},
		},
		DefaultChain: "default-chain",
	}
	r, err := NewResolver(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := r.Resolve(ResolveRequest{MinTrustTier: types.TrustPublic})
	if err != nil {
		t.Fatal(err)
	}
	if dec.ChainLabel != "default-chain" {
		t.Errorf("default_chain should fire when nothing else matches; got %q", dec.ChainLabel)
	}
	if dec.TriggerReason != "default_chain" {
		t.Errorf("reason should be 'default_chain'; got %q", dec.TriggerReason)
	}
}

// TestResolveChainTrustFloorRejectsWeakProvider — a chain whose
// step provider doesn't meet the request's MinTrustTier must NOT
// match even if the trigger fires. Resolver falls through.
func TestResolveChainTrustFloorRejectsWeakProvider(t *testing.T) {
	t.Parallel()
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{
			providerAt("weak", types.TrustPublic),
			providerAt("strong", types.TrustLocal),
		},
		Chains: []config.ChainConfig{
			{
				Label:   "weak-chain",
				Steps:   []config.ChainStepConfig{{Provider: "weak"}},
				Trigger: config.ChainTriggerConfig{Always: true},
			},
		},
	}
	r, _ := NewResolver(cfg)

	// Request requires local (strictest) — weak-chain should be
	// rejected; synthesised pick should grab 'strong'.
	dec, err := r.Resolve(ResolveRequest{MinTrustTier: types.TrustLocal})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if dec.ChainLabel == "weak-chain" {
		t.Error("weak-chain should have been rejected by trust floor")
	}
	if len(dec.Steps) != 1 || dec.Steps[0].Provider.Label != "strong" {
		t.Errorf("synthesised fallback should pick 'strong'; got %+v", dec.Steps)
	}
}

// TestResolveNoProviderMeetsFloor — when NO provider anywhere meets
// the trust floor, return ErrNoProvider. Caller surfaces this to
// the user as "no provider can handle this trust level".
func TestResolveNoProviderMeetsFloor(t *testing.T) {
	t.Parallel()
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{providerAt("only-public", types.TrustPublic)},
		Chains: []config.ChainConfig{
			{
				Label:   "chain",
				Steps:   []config.ChainStepConfig{{Provider: "only-public"}},
				Trigger: config.ChainTriggerConfig{Always: true},
			},
		},
	}
	r, _ := NewResolver(cfg)
	_, err := r.Resolve(ResolveRequest{MinTrustTier: types.TrustLocal})
	if err == nil {
		t.Fatal("expected ErrNoProvider")
	}
	if !errors.Is(err, ErrNoProvider) {
		t.Errorf("expected ErrNoProvider; got %v", err)
	}
}

// TestResolveChainMinTrustTierIsPerChainGate documents the chain-
// MinTrustTier semantics: it's a PER-CHAIN gate, not a global
// floor-upgrade. When a chain's floor can't be met, Resolver falls
// through to other chains / default / synthesised pick using the
// REQUEST's floor. Rationale in PLAN.md Phase 5.1: "each provider's
// trust_tier against the chain's min_trust_tier AND the scope's
// min_trust_tier" — i.e., both per-chain filter and absolute floor,
// not an upgrade mechanism.
func TestResolveChainMinTrustTierIsPerChainGate(t *testing.T) {
	t.Parallel()
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{
			providerAt("public-only", types.TrustPublic),
		},
		Chains: []config.ChainConfig{
			{
				Label:        "private-chain",
				Steps:        []config.ChainStepConfig{{Provider: "public-only"}},
				Trigger:      config.ChainTriggerConfig{Always: true},
				MinTrustTier: types.TrustPrivate,
			},
		},
	}
	r, _ := NewResolver(cfg)
	// Caller asks for public (loose); chain requires private but its
	// provider is public. Chain rejects; synthesis picks public-only
	// (meets request's public floor).
	dec, err := r.Resolve(ResolveRequest{MinTrustTier: types.TrustPublic})
	if err != nil {
		t.Fatalf("unexpected err (chain should fall through to synthesis): %v", err)
	}
	if dec.ChainLabel == "private-chain" {
		t.Error("private-chain should have been rejected — its floor couldn't be met")
	}
	if len(dec.Steps) != 1 || dec.Steps[0].Provider.Label != "public-only" {
		t.Errorf("synthesis should pick public-only; got %+v", dec.Steps)
	}
}

// TestResolveSynthesisedPicksHighestTrust — when synthesising a
// fallback, grab the STRONGEST provider at or above the floor, not
// just the first one. Cheaper providers might be lower-trust.
func TestResolveSynthesisedPicksHighestTrust(t *testing.T) {
	t.Parallel()
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{
			providerAt("pub", types.TrustPublic),
			providerAt("priv", types.TrustPrivate),
			providerAt("loc", types.TrustLocal),
		},
		// No chains at all — every request synthesises.
	}
	r, _ := NewResolver(cfg)

	// Floor=public → should pick the STRONGEST (local).
	dec, err := r.Resolve(ResolveRequest{MinTrustTier: types.TrustPublic})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Steps[0].Provider.Label != "loc" {
		t.Errorf("synthesis should prefer strongest tier; got %q", dec.Steps[0].Provider.Label)
	}
}

func TestResolveTriggerReasonSurfacesDiagnostics(t *testing.T) {
	t.Parallel()
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{providerAt("p", types.TrustPublic)},
		Chains: []config.ChainConfig{
			{
				Label:   "ch",
				Steps:   []config.ChainStepConfig{{Provider: "p"}},
				Trigger: config.ChainTriggerConfig{MinComplexity: 60},
			},
		},
	}
	r, _ := NewResolver(cfg)
	dec, _ := r.Resolve(ResolveRequest{Complexity: 75, MinTrustTier: types.TrustPublic})
	if !strings.Contains(dec.TriggerReason, "complexity") {
		t.Errorf("trigger reason should mention complexity; got %q", dec.TriggerReason)
	}
	if !strings.Contains(dec.TriggerReason, "60") {
		t.Errorf("trigger reason should mention the threshold; got %q", dec.TriggerReason)
	}
}

func TestResolveMultipleStepsPreserveRoles(t *testing.T) {
	t.Parallel()
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{
			providerAt("writer", types.TrustPublic),
			providerAt("reviewer", types.TrustPublic),
		},
		Chains: []config.ChainConfig{
			{
				Label: "writer-reviewer",
				Steps: []config.ChainStepConfig{
					{Provider: "writer", Role: "primary"},
					{Provider: "reviewer", Role: "reviewer", PromptTemplate: "critique: {input}"},
				},
				Trigger: config.ChainTriggerConfig{Always: true},
			},
		},
	}
	r, _ := NewResolver(cfg)
	dec, _ := r.Resolve(ResolveRequest{MinTrustTier: types.TrustPublic})
	if len(dec.Steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(dec.Steps))
	}
	if dec.Steps[0].Role != "primary" {
		t.Errorf("step[0].Role = %q, want 'primary'", dec.Steps[0].Role)
	}
	if dec.Steps[1].Role != "reviewer" {
		t.Errorf("step[1].Role = %q, want 'reviewer'", dec.Steps[1].Role)
	}
	if dec.Steps[1].PromptTemplate != "critique: {input}" {
		t.Errorf("PromptTemplate didn't propagate: %q", dec.Steps[1].PromptTemplate)
	}
}

func TestResolveEmptyMinTrustTierIsPublic(t *testing.T) {
	t.Parallel()
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{providerAt("p", types.TrustPublic)},
		Chains: []config.ChainConfig{
			{
				Label:   "c",
				Steps:   []config.ChainStepConfig{{Provider: "p"}},
				Trigger: config.ChainTriggerConfig{Always: true},
			},
		},
	}
	r, _ := NewResolver(cfg)
	// Empty MinTrustTier should be treated as public (most permissive).
	dec, err := r.Resolve(ResolveRequest{})
	if err != nil {
		t.Fatalf("empty MinTrustTier should default; got err %v", err)
	}
	if dec.ChainLabel != "c" {
		t.Errorf("chain should match with default floor; got %q", dec.ChainLabel)
	}
}

// TestResolveProviderCopyIsIndependent — a caller that mutates
// its copy of the resolved provider shouldn't affect the resolver's
// stored config. Important because config reload may be streaming
// through the resolver.
func TestResolveProviderCopyIsIndependent(t *testing.T) {
	t.Parallel()
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{providerAt("p", types.TrustPublic)},
		Chains: []config.ChainConfig{
			{
				Label:   "c",
				Steps:   []config.ChainStepConfig{{Provider: "p"}},
				Trigger: config.ChainTriggerConfig{Always: true},
			},
		},
	}
	r, _ := NewResolver(cfg)

	dec1, _ := r.Resolve(ResolveRequest{MinTrustTier: types.TrustPublic})
	dec1.Steps[0].Provider.Model = "MUTATED-BY-CALLER"

	dec2, _ := r.Resolve(ResolveRequest{MinTrustTier: types.TrustPublic})
	if dec2.Steps[0].Provider.Model == "MUTATED-BY-CALLER" {
		t.Error("caller mutation leaked into resolver state")
	}
}

// TestResolveOverlapDomainsDiagnostic confirms the trigger-reason
// names *which* domains overlapped, not just "matched". Helps
// operators debug why a surprising chain fired.
func TestResolveOverlapDomainsDiagnostic(t *testing.T) {
	t.Parallel()
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{providerAt("p", types.TrustPublic)},
		Chains: []config.ChainConfig{
			{
				Label:   "ch",
				Steps:   []config.ChainStepConfig{{Provider: "p"}},
				Trigger: config.ChainTriggerConfig{Domains: []string{"finance", "legal"}},
			},
		},
	}
	r, _ := NewResolver(cfg)
	dec, _ := r.Resolve(ResolveRequest{
		Domains:      []string{"finance", "unrelated"},
		MinTrustTier: types.TrustPublic,
	})
	if !slices.Contains(strings.Split(dec.TriggerReason, " "), "domain-overlap=[finance]") {
		// The exact wrap isn't guaranteed (contains instead):
		if !strings.Contains(dec.TriggerReason, "finance") {
			t.Errorf("expected 'finance' in trigger reason; got %q", dec.TriggerReason)
		}
	}
}
