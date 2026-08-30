package compute

import (
	"testing"
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
