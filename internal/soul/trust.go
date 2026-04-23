package soul

import (
	"fmt"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// ProviderTrustTier is the subset of a provider config the soul
// layer actually reads. Kept narrow so internal/soul doesn't
// depend on pkg/config's fuller ProviderConfig shape.
type ProviderTrustTier struct {
	Label     string
	TrustTier types.TrustTier
}

// ValidateProviderTier checks a provider against the soul's
// min_trust_tier floor. Returns nil when the floor is unset
// (operator hasn't opted in) OR when the provider meets the floor.
// A below-floor provider returns a descriptive error suitable for
// surfacing at boot so operators see which provider and which
// soul-configured tier collided.
func ValidateProviderTier(s *Soul, provider ProviderTrustTier) error {
	if s == nil {
		return nil
	}
	floor := s.Config.MinTrustTier
	if floor == "" {
		return nil
	}
	if !floor.IsValid() {
		return fmt.Errorf("soul: min_trust_tier %q is not a recognised tier", floor)
	}
	if !provider.TrustTier.IsValid() {
		return fmt.Errorf("soul: provider %q has invalid trust_tier %q",
			provider.Label, provider.TrustTier)
	}
	if !provider.TrustTier.AtLeast(floor) {
		return fmt.Errorf(
			"soul: provider %q trust_tier=%q is below soul floor %q — configure a "+
				"higher-trust provider or lower the SOUL.md min_trust_tier",
			provider.Label, provider.TrustTier, floor)
	}
	return nil
}
