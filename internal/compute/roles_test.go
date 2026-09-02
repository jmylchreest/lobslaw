package compute

import (
	"testing"
	"time"
)

// newStubProvider returns a minimal LLMProvider for role-map tests.
// Backed by MockProvider rather than a hand-rolled stub so it stays
// in step with the LLMProvider interface.
func newStubProvider(t *testing.T, id string) LLMProvider {
	t.Helper()
	return NewMockProvider(MockResponse{Content: id})
}

func TestRoleMapRequiresMain(t *testing.T) {
	t.Parallel()
	if _, err := NewRoleMap(nil, nil); err == nil {
		t.Error("nil main should fail")
	}
}

func TestRoleMapMainFallsThrough(t *testing.T) {
	t.Parallel()
	main := newStubProvider(t, "main")
	rm, err := NewRoleMap(main, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []Role{RoleMain, RolePreflight, RoleSummariser} {
		if rm.For(role) != main {
			t.Errorf("role %q didn't fall through to main", role)
		}
	}
}

func TestRoleMapExplicitPreflight(t *testing.T) {
	t.Parallel()
	main := newStubProvider(t, "main")
	pre := newStubProvider(t, "preflight")
	rm, _ := NewRoleMap(main, map[Role]LLMProvider{RolePreflight: pre})
	if rm.For(RolePreflight) != pre {
		t.Error("preflight override failed")
	}
	// Summariser falls through to main.
	if rm.For(RoleSummariser) != main {
		t.Error("summariser should fall back to main")
	}
}

// A deadline belongs to the ROLE, and does not travel down the provider
// fallback chain. The classifier runs on the same model as the turn and
// can afford two orders of magnitude less time — that is the whole
// reason this is not a per-provider setting.
func TestRoleMapTimeoutFor(t *testing.T) {
	t.Parallel()
	rm, err := NewRoleMap(newStubProvider(t, "main"), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Nothing configured: zero, meaning "use your own constant".
	if got := rm.TimeoutFor(RoleCommandRisk); got != 0 {
		t.Errorf("unconfigured = %v, want 0", got)
	}

	rm.SetTimeouts(60*time.Second, map[Role]time.Duration{
		RoleCommandRisk: 15 * time.Second,
		RoleReview:      5 * time.Minute,
	})

	if got := rm.TimeoutFor(RoleCommandRisk); got != 15*time.Second {
		t.Errorf("command_risk = %v, want 15s", got)
	}
	if got := rm.TimeoutFor(RoleReview); got != 5*time.Minute {
		t.Errorf("review = %v, want 5m", got)
	}
	// A role with no timeout of its own takes the global default —
	// NOT the timeout of the role its provider falls back to.
	if got := rm.TimeoutFor(RolePreflight); got != 60*time.Second {
		t.Errorf("preflight = %v, want the 60s default", got)
	}
	if got := rm.TimeoutFor(RoleSummariser); got != 60*time.Second {
		t.Errorf("summariser = %v, want the 60s default", got)
	}
}

// A nil map is usable, for the same reason a nil Judge is: callers
// should not branch on whether an operator configured anything.
func TestNilRoleMapTimeout(t *testing.T) {
	t.Parallel()
	var rm *RoleMap
	if got := rm.TimeoutFor(RoleMain); got != 0 {
		t.Errorf("nil map returned %v, want 0", got)
	}
}

// Zero from config means "no opinion", never "no time at all".
// Reading it as a deadline would cancel the call before it was made.
func TestOrDefault(t *testing.T) {
	t.Parallel()
	if got := orDefault(0, 8*time.Second); got != 8*time.Second {
		t.Errorf("orDefault(0, 8s) = %v, want the fallback", got)
	}
	if got := orDefault(15*time.Second, 8*time.Second); got != 15*time.Second {
		t.Errorf("orDefault(15s, 8s) = %v, want the configured value", got)
	}
	if got := orDefault(-1, 8*time.Second); got != 8*time.Second {
		t.Errorf("a negative duration was treated as a deadline: %v", got)
	}
}
