package egress

import (
	"testing"
)

func TestDefaultProviderIsNoop(t *testing.T) {
	t.Parallel()
	c := For("test-role")
	if c.Role() != "test-role" {
		t.Errorf("role = %q, want test-role", c.Role())
	}
	if c.HTTPClient() == nil {
		t.Error("http client should be non-nil even before SetActiveProvider")
	}
}

func TestForSkillPrefixesRole(t *testing.T) {
	t.Parallel()
	c := ForSkill("gws-workspace")
	if c.Role() != "skill/gws-workspace" {
		t.Errorf("role = %q, want skill/gws-workspace", c.Role())
	}
}

func TestForMCPPrefixesRole(t *testing.T) {
	t.Parallel()
	c := ForMCP("minimax")
	if c.Role() != "mcp/minimax" {
		t.Errorf("role = %q, want mcp/minimax", c.Role())
	}
}

func TestForOAuthPrefixesRole(t *testing.T) {
	t.Parallel()
	c := ForOAuth("google")
	if c.Role() != "oauth/google" {
		t.Errorf("role = %q, want oauth/google", c.Role())
	}
}

func TestSetActiveProviderSwapsImpl(t *testing.T) {
	// Not parallel — mutates global active provider.
	original := activeProvider()
	t.Cleanup(func() { SetActiveProvider(original) })

	swapped := &fakeProvider{role: "swapped"}
	SetActiveProvider(swapped)

	c := For("ignored")
	if c.Role() != "swapped" {
		t.Errorf("active provider not swapped: role = %q", c.Role())
	}
}

func TestSetActiveProviderNilFallsBackToNoop(t *testing.T) {
	original := activeProvider()
	t.Cleanup(func() { SetActiveProvider(original) })

	SetActiveProvider(nil)
	c := For("test-role")
	if c == nil || c.HTTPClient() == nil {
		t.Error("nil provider should fall back to noop, not panic")
	}
}

type fakeProvider struct{ role string }

func (f *fakeProvider) For(_ string) Client { return &noopClient{role: f.role} }
