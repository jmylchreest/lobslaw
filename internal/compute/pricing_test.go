package compute

import (
	"math"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func approxEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

func TestResolvePricingExactMatch(t *testing.T) {
	t.Parallel()
	p := config.ProviderConfig{Model: "gpt-4o-mini"}
	pricing, found := ResolvePricing(p)
	if !found {
		t.Fatal("expected to find built-in for gpt-4o-mini")
	}
	want := BuiltinPricing["gpt-4o-mini"]
	if pricing != want {
		t.Errorf("got %+v, want %+v", pricing, want)
	}
}

func TestResolvePricingCaseInsensitive(t *testing.T) {
	t.Parallel()
	p := config.ProviderConfig{Model: "GPT-4O-MINI"}
	_, found := ResolvePricing(p)
	if !found {
		t.Error("case-insensitive match should find built-in")
	}
}

// TestResolvePricingPrefixMatch — providers often report dated
// model IDs (gpt-4o-mini-2024-08-06); the table should match the
// base family. Longest-matching-prefix wins so gpt-4o-mini beats
// gpt-4o for gpt-4o-mini-2024-XX.
func TestResolvePricingPrefixMatch(t *testing.T) {
	t.Parallel()
	p := config.ProviderConfig{Model: "gpt-4o-mini-2024-08-06"}
	pricing, found := ResolvePricing(p)
	if !found {
		t.Fatal("prefix match should find built-in")
	}
	if pricing != BuiltinPricing["gpt-4o-mini"] {
		t.Errorf("longest prefix should win; got %+v", pricing)
	}
}

func TestResolvePricingUnknownReturnsNotFound(t *testing.T) {
	t.Parallel()
	p := config.ProviderConfig{Model: "obscure-model-xyz"}
	_, found := ResolvePricing(p)
	if found {
		t.Error("unknown model should not be found in built-in table")
	}
}

// TestResolvePricingOverrideSupersedesPerField — any non-zero
// override field beats the built-in; zero fields fall through.
// Matters for operators on negotiated rates.
func TestResolvePricingOverrideSupersedesPerField(t *testing.T) {
	t.Parallel()
	p := config.ProviderConfig{
		Model: "gpt-4o",
		Pricing: types.ProviderPricing{
			InputUSDPer1K: 0.002, // override (negotiated lower rate)
			// OutputUSDPer1K left zero → falls through to built-in 0.01
		},
	}
	pricing, found := ResolvePricing(p)
	if !found {
		t.Fatal("expected found=true")
	}
	if pricing.InputUSDPer1K != 0.002 {
		t.Errorf("override not applied: got %f", pricing.InputUSDPer1K)
	}
	if pricing.OutputUSDPer1K != BuiltinPricing["gpt-4o"].OutputUSDPer1K {
		t.Errorf("zero field should fall through; got %f", pricing.OutputUSDPer1K)
	}
}

func TestResolvePricingOverrideOnly(t *testing.T) {
	t.Parallel()
	// No built-in for this model; the override itself supplies pricing.
	p := config.ProviderConfig{
		Model: "custom-model",
		Pricing: types.ProviderPricing{
			InputUSDPer1K:  0.001,
			OutputUSDPer1K: 0.005,
		},
	}
	pricing, found := ResolvePricing(p)
	if !found {
		t.Fatal("override alone should be enough to establish pricing")
	}
	if pricing.InputUSDPer1K != 0.001 || pricing.OutputUSDPer1K != 0.005 {
		t.Errorf("override values didn't survive: %+v", pricing)
	}
}

func TestResolvePricingEmptyModelAndNoOverride(t *testing.T) {
	t.Parallel()
	p := config.ProviderConfig{}
	_, found := ResolvePricing(p)
	if found {
		t.Error("empty config should yield found=false")
	}
}

func TestEstimateCostBasic(t *testing.T) {
	t.Parallel()
	pricing := types.ProviderPricing{
		InputUSDPer1K:  0.001,
		OutputUSDPer1K: 0.002,
	}
	u := Usage{PromptTokens: 1000, CompletionTokens: 500}
	// 1000 * 0.001/1000 + 500 * 0.002/1000 = 0.001 + 0.001 = 0.002
	got := EstimateCost(u, pricing)
	if !approxEqual(got, 0.002, 1e-9) {
		t.Errorf("got %f, want 0.002", got)
	}
}

// TestEstimateCostCachedTokensDiscounted — cached tokens should
// bill at CachedUSDPer1K, NOT at InputUSDPer1K. Real savings are
// 75-90% off regular input rates — the agent loop relies on this
// to track the actual dollar impact of prompt caching.
func TestEstimateCostCachedTokensDiscounted(t *testing.T) {
	t.Parallel()
	pricing := types.ProviderPricing{
		InputUSDPer1K:  0.01,
		OutputUSDPer1K: 0.02,
		CachedUSDPer1K: 0.001, // 10x cheaper
	}
	u := Usage{
		PromptTokens:     1000,
		CachedTokens:     900, // most of the prompt was cached
		CompletionTokens: 100,
	}
	// non-cached: 100 * 0.01/1000 = 0.001
	// cached:     900 * 0.001/1000 = 0.0009
	// output:     100 * 0.02/1000 = 0.002
	// total = 0.0039
	got := EstimateCost(u, pricing)
	if !approxEqual(got, 0.0039, 1e-9) {
		t.Errorf("got %f, want 0.0039", got)
	}
}

// TestEstimateCostCachedGreaterThanPromptIsClamped — provider
// reports CachedTokens > PromptTokens. Treat non-cached as zero
// rather than letting the cost estimate go negative.
func TestEstimateCostCachedGreaterThanPromptIsClamped(t *testing.T) {
	t.Parallel()
	pricing := types.ProviderPricing{
		InputUSDPer1K:  0.01,
		CachedUSDPer1K: 0.001,
	}
	u := Usage{
		PromptTokens: 100,
		CachedTokens: 150, // nonsensical, but be robust
	}
	got := EstimateCost(u, pricing)
	// non-cached portion clamped to 0; cached billed at 150 * 0.001/1000.
	want := 150.0 * 0.001 / 1000.0
	if !approxEqual(got, want, 1e-9) {
		t.Errorf("got %f, want %f", got, want)
	}
}

func TestEstimateCostZeroPricingIsZeroCost(t *testing.T) {
	t.Parallel()
	got := EstimateCost(Usage{PromptTokens: 100000, CompletionTokens: 100000}, types.ProviderPricing{})
	if got != 0 {
		t.Errorf("zero pricing should yield zero cost; got %f", got)
	}
}

func TestEstimateCostZeroUsage(t *testing.T) {
	t.Parallel()
	pricing := types.ProviderPricing{InputUSDPer1K: 10, OutputUSDPer1K: 20}
	got := EstimateCost(Usage{}, pricing)
	if got != 0 {
		t.Errorf("zero usage should yield zero cost; got %f", got)
	}
}

func TestRecordCostComposes(t *testing.T) {
	t.Parallel()
	pricing := types.ProviderPricing{InputUSDPer1K: 0.001, OutputUSDPer1K: 0.002}
	usage := Usage{PromptTokens: 1000, CompletionTokens: 500}
	rec := RecordCost("openrouter", "gpt-4o-mini", usage, pricing)
	if rec.ProviderLabel != "openrouter" || rec.Model != "gpt-4o-mini" {
		t.Errorf("label/model not captured: %+v", rec)
	}
	if rec.Usage != usage {
		t.Errorf("usage not captured: %+v", rec.Usage)
	}
	if !approxEqual(rec.CostUSD, 0.002, 1e-9) {
		t.Errorf("cost: got %f, want 0.002", rec.CostUSD)
	}
}

// TestBuiltinPricingSanity — cheap smoke-test that the built-in
// table has all the rows we reference in project docs. If a row
// vanishes the doc drifts; better to fail loudly at test time.
func TestBuiltinPricingSanity(t *testing.T) {
	t.Parallel()
	required := []string{
		"gpt-4o", "gpt-4o-mini",
		"claude-opus-4-7", "claude-sonnet-4-6", "claude-haiku-4-5",
	}
	for _, m := range required {
		if _, ok := BuiltinPricing[m]; !ok {
			t.Errorf("BuiltinPricing missing row %q — did the default table get thinned?", m)
		}
	}
}
