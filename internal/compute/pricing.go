package compute

import (
	"strings"

	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// BuiltinPricing is the baked-in default table keyed by lowercased
// model name. Operators override per-provider via ProviderConfig.Pricing.
//
// Prices reflect standard API pricing as of 2026-04; operators with
// discounts / dedicated instances will override. Values are USD per
// 1,000 tokens; zero for CachedUSDPer1K means the provider doesn't
// offer native prompt caching (or we don't track it).
//
// Naming convention: match the model string reported by the provider
// as closely as possible, case-folded. Lookup tries exact match first,
// then prefix match (so "claude-sonnet-4-6" matches "claude-sonnet").
//
// Drift warning: hardcoded prices drift as providers change rates.
// Auto-refresh from provider pricing endpoints is tracked in DEFERRED.md
// — until then, operators notice drift when their per-turn dollar
// estimate diverges from the provider's invoice.
var BuiltinPricing = map[string]types.ProviderPricing{
	// OpenAI
	"gpt-4o":        {InputUSDPer1K: 0.0025, OutputUSDPer1K: 0.01, CachedUSDPer1K: 0.00125},
	"gpt-4o-mini":   {InputUSDPer1K: 0.00015, OutputUSDPer1K: 0.0006, CachedUSDPer1K: 0.000075},
	"gpt-4-turbo":   {InputUSDPer1K: 0.01, OutputUSDPer1K: 0.03},
	"gpt-3.5-turbo": {InputUSDPer1K: 0.0005, OutputUSDPer1K: 0.0015},

	// Anthropic (via OpenAI-compat gateway or direct once supported)
	"claude-opus-4-7":   {InputUSDPer1K: 0.015, OutputUSDPer1K: 0.075, CachedUSDPer1K: 0.0015},
	"claude-sonnet-4-6": {InputUSDPer1K: 0.003, OutputUSDPer1K: 0.015, CachedUSDPer1K: 0.0003},
	"claude-haiku-4-5":  {InputUSDPer1K: 0.0008, OutputUSDPer1K: 0.004, CachedUSDPer1K: 0.00008},

	// Local (Ollama) — assumed zero cost; operators running self-hosted
	// can override to account for GPU-hour amortisation if needed.
	"llama3":   {},
	"llama3.1": {},
	"mistral":  {},
	"qwen":     {},
}

// ResolvePricing merges a provider's override with the built-in
// defaults. Field-wise precedence: any non-zero field on the override
// wins; zero fields fall back to the built-in entry for the provider's
// Model. Returns a zero-value ProviderPricing when neither source has
// an entry — which means "cost unknown; attribute as zero for now".
//
// Callers who need to distinguish "known zero" (e.g. local LLM) from
// "unknown" check the returned (pricing, found) pair.
func ResolvePricing(p config.ProviderConfig) (pricing types.ProviderPricing, found bool) {
	builtin, hasBuiltin := lookupBuiltin(p.Model)

	override := p.Pricing
	anyOverride := override.InputUSDPer1K > 0 || override.OutputUSDPer1K > 0 || override.CachedUSDPer1K > 0

	if !hasBuiltin && !anyOverride {
		return types.ProviderPricing{}, false
	}

	// Start from builtin; let any non-zero override field supersede.
	result := builtin
	if override.InputUSDPer1K > 0 {
		result.InputUSDPer1K = override.InputUSDPer1K
	}
	if override.OutputUSDPer1K > 0 {
		result.OutputUSDPer1K = override.OutputUSDPer1K
	}
	if override.CachedUSDPer1K > 0 {
		result.CachedUSDPer1K = override.CachedUSDPer1K
	}
	return result, true
}

// lookupBuiltin checks BuiltinPricing by exact lowercase match,
// then by prefix. Prefix match is lenient on purpose — providers
// often report version-specific model IDs ("gpt-4o-2024-08-06") that
// should bill against the base rate.
func lookupBuiltin(model string) (types.ProviderPricing, bool) {
	if model == "" {
		return types.ProviderPricing{}, false
	}
	lower := strings.ToLower(model)
	if p, ok := BuiltinPricing[lower]; ok {
		return p, true
	}
	// Prefix match. Iterate keys so the longest matching prefix wins
	// — means "gpt-4o-mini" beats "gpt-4o" for "gpt-4o-mini-2024-XX".
	var bestKey string
	var best types.ProviderPricing
	for key, p := range BuiltinPricing {
		if !strings.HasPrefix(lower, key) {
			continue
		}
		if len(key) > len(bestKey) {
			bestKey = key
			best = p
		}
	}
	if bestKey != "" {
		return best, true
	}
	return types.ProviderPricing{}, false
}

// EstimateCost computes the USD cost of one LLM call given its
// Usage and the provider's effective pricing. Returns 0 when
// pricing is unknown (distinguishable from "known zero" via
// ResolvePricing's found boolean when the caller cares).
//
// Formula: cached tokens bill at cached rate; the rest of prompt
// tokens at input rate; completion tokens at output rate. Uses
// float64 throughout — per-call dollar values are tiny and an
// integer-cent approach would lose meaningful precision for
// sub-penny calls.
func EstimateCost(usage Usage, pricing types.ProviderPricing) float64 {
	nonCached := usage.PromptTokens - usage.CachedTokens
	if nonCached < 0 {
		// Defensive: provider reported cached > total prompt. Treat as
		// all-cached rather than negative.
		nonCached = 0
	}
	cost := 0.0
	cost += float64(nonCached) * pricing.InputUSDPer1K / 1000.0
	cost += float64(usage.CachedTokens) * pricing.CachedUSDPer1K / 1000.0
	cost += float64(usage.CompletionTokens) * pricing.OutputUSDPer1K / 1000.0
	return cost
}

// CostRecord is one attributed call — usage + computed cost + the
// provider label that billed. Emitted by the agent loop (Phase 5.4)
// and accumulated on the TurnBudget (Phase 5.3). Retained for audit.
type CostRecord struct {
	ProviderLabel string
	Model         string
	Usage         Usage
	CostUSD       float64
}

// RecordCost builds a CostRecord from a Usage + resolved provider.
// Convenience wrapper — the agent loop calls this per LLM round-trip.
func RecordCost(providerLabel, model string, usage Usage, pricing types.ProviderPricing) CostRecord {
	return CostRecord{
		ProviderLabel: providerLabel,
		Model:         model,
		Usage:         usage,
		CostUSD:       EstimateCost(usage, pricing),
	}
}
