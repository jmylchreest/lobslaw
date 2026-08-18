package compute

import (
	"github.com/jmylchreest/lobslaw/internal/modelsdev"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Pricing from the catalogue, so a rate card is data rather than
// something an operator retypes.
//
// Every price hand-written into this cluster's config already existed
// in models.dev. Two matched to the digit, which made the typing
// pointless; the third was WRONG — a flat-rate token plan given
// invented per-token rates, so every trace reported a cost that was
// not being incurred. Typing a number the registry already holds has
// no upside and one failure mode.

// perMillionToPerThousand converts the catalogue's unit to ours.
//
// The catalogue quotes USD per MILLION tokens; ProviderPricing is per
// THOUSAND. Getting this backwards is a factor of a million in a
// number nobody eyeballs, and it would look plausible either way on a
// single cheap turn — which is exactly why it is one named function
// with a test rather than three inline divisions.
const perMillionToPerThousand = 1000.0

// PricingFromModel converts a catalogue entry's rate card.
//
// A model with no cost block yields the zero value, which is the same
// thing an unpriced provider has always meant: the quantity is still
// recorded, the money column reads zero. That is the correct answer
// for a plan-billed provider, and the catalogue says 0 for exactly
// those.
func PricingFromModel(m modelsdev.Model) types.ProviderPricing {
	return types.ProviderPricing{
		InputUSDPer1K:  m.Cost.Input / perMillionToPerThousand,
		OutputUSDPer1K: m.Cost.Output / perMillionToPerThousand,
		CachedUSDPer1K: m.Cost.CacheRead / perMillionToPerThousand,
	}
}

// MergePricing applies declared-precedence, matching how capabilities
// merge.
//
// WHOLE-BLOCK, not field-by-field. An operator who wrote a rate card
// meant that card: filling a missing cached rate from the catalogue
// would mix two sources into one number and neither would be
// answerable for it. Declared silence about caching means "we are not
// pricing it", not "look it up".
//
// Declared is anything non-zero — there is no way to distinguish
// "unset" from "deliberately zero" in a koanf struct, and a
// deliberate zero and an absent block produce identical behaviour
// anyway.
func MergePricing(declared, discovered types.ProviderPricing) types.ProviderPricing {
	if !PricingIsZero(declared) {
		return declared
	}
	return discovered
}

// PricingIsZero reports whether a rate card prices nothing.
//
// A helper rather than a == comparison: ProviderPricing carries a map
// for non-token units, so the struct is not comparable and `==` does
// not compile. Written once here so no caller reaches for reflect.
func PricingIsZero(p types.ProviderPricing) bool {
	return p.InputUSDPer1K == 0 && p.OutputUSDPer1K == 0 &&
		p.CachedUSDPer1K == 0 && len(p.UnitUSD) == 0
}
