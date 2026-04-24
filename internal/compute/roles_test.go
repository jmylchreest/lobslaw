package compute

import (
	"testing"
)

// stubProvider is a trivial LLMProvider for role-map tests.
type stubProvider struct{ id string }

func (s *stubProvider) Chat(_ any, _ any) (any, error) { return nil, nil }

// Match the LLMProvider interface. Because Chat has a specific
// signature we can't satisfy trivially, reuse a MockProvider.
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
	for _, role := range []Role{RoleMain, RolePreflight, RoleReranker, RoleSummariser} {
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
	// Reranker falls through to preflight when set.
	if rm.For(RoleReranker) != pre {
		t.Error("reranker should fall back to preflight")
	}
	// Summariser falls through to main.
	if rm.For(RoleSummariser) != main {
		t.Error("summariser should fall back to main")
	}
}
