package soul

import (
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestValidateProviderTierNilSoul(t *testing.T) {
	t.Parallel()
	if err := ValidateProviderTier(nil, ProviderTrustTier{
		Label: "p", TrustTier: types.TrustPublic,
	}); err != nil {
		t.Errorf("nil soul should be no-op; got %v", err)
	}
}

func TestValidateProviderTierUnsetFloor(t *testing.T) {
	t.Parallel()
	s := DefaultSoul() // MinTrustTier is unset
	if err := ValidateProviderTier(s, ProviderTrustTier{
		Label: "p", TrustTier: types.TrustPublic,
	}); err != nil {
		t.Errorf("unset floor should be no-op; got %v", err)
	}
}

func TestValidateProviderTierAtFloorOK(t *testing.T) {
	t.Parallel()
	s := DefaultSoul()
	s.Config.MinTrustTier = types.TrustPrivate
	if err := ValidateProviderTier(s, ProviderTrustTier{
		Label: "private-provider", TrustTier: types.TrustPrivate,
	}); err != nil {
		t.Errorf("provider at floor should pass; got %v", err)
	}
}

func TestValidateProviderTierAboveFloorOK(t *testing.T) {
	t.Parallel()
	s := DefaultSoul()
	s.Config.MinTrustTier = types.TrustPrivate
	if err := ValidateProviderTier(s, ProviderTrustTier{
		Label: "local-llm", TrustTier: types.TrustLocal,
	}); err != nil {
		t.Errorf("local (stronger) should pass private floor; got %v", err)
	}
}

func TestValidateProviderTierBelowFloorRejects(t *testing.T) {
	t.Parallel()
	s := DefaultSoul()
	s.Config.MinTrustTier = types.TrustPrivate
	err := ValidateProviderTier(s, ProviderTrustTier{
		Label: "public-provider", TrustTier: types.TrustPublic,
	})
	if err == nil {
		t.Fatal("below-floor provider should reject")
	}
	if !strings.Contains(err.Error(), "below soul floor") {
		t.Errorf("error should mention the floor; got %v", err)
	}
	// The operator needs both names in the message to fix it.
	if !strings.Contains(err.Error(), "public-provider") {
		t.Errorf("error should name the provider; got %v", err)
	}
}

func TestValidateProviderTierInvalidFloorRejects(t *testing.T) {
	t.Parallel()
	s := DefaultSoul()
	s.Config.MinTrustTier = types.TrustTier("garbage")
	err := ValidateProviderTier(s, ProviderTrustTier{
		Label: "p", TrustTier: types.TrustPublic,
	})
	if err == nil {
		t.Fatal("invalid floor should reject")
	}
}

func TestValidateProviderTierInvalidProviderTierRejects(t *testing.T) {
	t.Parallel()
	s := DefaultSoul()
	s.Config.MinTrustTier = types.TrustPrivate
	err := ValidateProviderTier(s, ProviderTrustTier{
		Label: "p", TrustTier: types.TrustTier(""),
	})
	if err == nil {
		t.Fatal("invalid provider tier should reject")
	}
}
