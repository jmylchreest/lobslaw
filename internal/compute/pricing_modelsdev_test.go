package compute

import (
	"testing"

	"github.com/jmylchreest/lobslaw/internal/modelsdev"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// THE UNIT BUG. The catalogue quotes USD per MILLION tokens;
// ProviderPricing is per THOUSAND. Getting it backwards is a factor
// of a million in a number nobody eyeballs, and on one cheap turn it
// looks plausible either way.
func TestTheCatalogueRateIsConvertedFromPerMillionToPerThousand(t *testing.T) {
	t.Parallel()
	// gpt-4o-mini as models.dev actually lists it.
	got := PricingFromModel(modelsdev.Model{
		Cost: modelsdev.Cost{Input: 0.15, Output: 0.6, CacheRead: 0.075},
	})
	want := types.ProviderPricing{
		InputUSDPer1K:  0.00015,
		OutputUSDPer1K: 0.0006,
		CachedUSDPer1K: 0.000075,
	}
	if !samePricing(got, want) {
		t.Errorf("pricing = %+v, want %+v", got, want)
	}
}

// A plan-billed provider has no marginal rate, and the catalogue says
// so with zeros. That must stay zero rather than become a guess —
// this cluster ran for a while reporting invented per-token costs on
// a flat-rate plan.
func TestAFlatRatePlanStaysZero(t *testing.T) {
	t.Parallel()
	got := PricingFromModel(modelsdev.Model{Cost: modelsdev.Cost{}})
	if !PricingIsZero(got) {
		t.Errorf("a costless catalogue entry produced %+v", got)
	}
}

// Whole-block, not field-by-field. An operator who wrote a rate card
// meant that card; filling a missing cached rate from the catalogue
// would mix two sources into one number and neither would be
// answerable for it.
func TestDeclaredPricingWinsWholesale(t *testing.T) {
	t.Parallel()
	declared := types.ProviderPricing{InputUSDPer1K: 0.001, OutputUSDPer1K: 0.002}
	discovered := types.ProviderPricing{InputUSDPer1K: 9, OutputUSDPer1K: 9, CachedUSDPer1K: 9}

	got := MergePricing(declared, discovered)
	if !samePricing(got, declared) {
		t.Errorf("merged = %+v, want the declared card verbatim", got)
	}
	if got.CachedUSDPer1K != 0 {
		t.Error("a cached rate was borrowed from the catalogue into a hand-written card")
	}
}

func TestAnEmptyDeclarationTakesTheCatalogue(t *testing.T) {
	t.Parallel()
	discovered := types.ProviderPricing{InputUSDPer1K: 0.00015, OutputUSDPer1K: 0.0006}
	if got := MergePricing(types.ProviderPricing{}, discovered); !samePricing(got, discovered) {
		t.Errorf("merged = %+v, want the catalogue card", got)
	}
}

// A non-token rate card (video by the second, images by the image) is
// a declaration too, and must not be overwritten by token prices.
func TestAUnitOnlyCardCountsAsDeclared(t *testing.T) {
	t.Parallel()
	declared := types.ProviderPricing{UnitUSD: map[string]float64{"image": 0.04}}
	discovered := types.ProviderPricing{InputUSDPer1K: 9}
	got := MergePricing(declared, discovered)
	if got.InputUSDPer1K != 0 || len(got.UnitUSD) != 1 {
		t.Errorf("merged = %+v; a unit-priced provider was given token rates", got)
	}
}
