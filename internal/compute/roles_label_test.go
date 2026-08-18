package compute

import "testing"

// list_providers reported labels, tiers and backup pointers and
// nothing about PURPOSE. Asked which model handled the fast
// classification path, the agent had no way to know and answered from
// the shape of the list — naming a provider that served no role at
// all. A confident wrong answer about the node's own configuration.

func labelledMap(t *testing.T, explicit map[Role]string) *RoleMap {
	t.Helper()
	clients := map[Role]LLMProvider{}
	for role := range explicit {
		clients[role] = NewMockProvider(MockResponse{Content: "x"})
	}
	rm, err := NewRoleMapWithLabels(
		NewMockProvider(MockResponse{Content: "main"}), clients, "main-provider", explicit)
	if err != nil {
		t.Fatal(err)
	}
	return rm
}

// The RESOLVED provider, not the configured one. Reporting only what
// config named would tell an operator their fast path is unconfigured
// when it is in fact running on the main model — which is exactly
// what they are asking in order to fix.
func TestAnUnsetRoleReportsTheProviderItActuallyFallsBackTo(t *testing.T) {
	t.Parallel()
	rm := labelledMap(t, map[Role]string{})
	for _, role := range []Role{RoleMain, RolePreflight, RoleReranker, RoleSummariser, RoleReview} {
		if got := rm.LabelFor(role); got != "main-provider" {
			t.Errorf("%s resolved to %q, want the main provider", role, got)
		}
	}
}

func TestAnExplicitRoleReportsItsOwnProvider(t *testing.T) {
	t.Parallel()
	rm := labelledMap(t, map[Role]string{RolePreflight: "tiny"})
	if got := rm.LabelFor(RolePreflight); got != "tiny" {
		t.Errorf("preflight = %q, want tiny", got)
	}
}

// Reranker is the only two-step fallback: preflight first, then main.
// Getting this wrong would name main while the turn used preflight.
func TestTheRerankerFallsBackThroughPreflightNotStraightToMain(t *testing.T) {
	t.Parallel()
	rm := labelledMap(t, map[Role]string{RolePreflight: "tiny"})
	if got := rm.LabelFor(RoleReranker); got != "tiny" {
		t.Errorf("reranker = %q, want tiny (via preflight)", got)
	}
}

// THE INVARIANT. A label naming a provider other than the one the
// turn used is worse than no label: it is a wrong answer delivered
// with the authority of the node's own configuration. For and
// LabelFor must walk one chain, not two that happen to agree today.
func TestTheLabelAlwaysNamesTheProviderThatActuallyRuns(t *testing.T) {
	t.Parallel()
	for _, explicit := range []map[Role]string{
		{},
		{RolePreflight: "tiny"},
		{RoleSummariser: "big"},
		{RolePreflight: "tiny", RoleReranker: "other"},
		{RoleMain: "overridden", RolePreflight: "tiny"},
		{RolePreflight: "tiny", RoleSummariser: "big", RoleReranker: "other"},
	} {
		clients := map[Role]LLMProvider{}
		byLabel := map[string]LLMProvider{"main-provider": NewMockProvider(MockResponse{Content: "main"})}
		for role, label := range explicit {
			if _, ok := byLabel[label]; !ok {
				byLabel[label] = NewMockProvider(MockResponse{Content: label})
			}
			clients[role] = byLabel[label]
		}
		rm, err := NewRoleMapWithLabels(byLabel["main-provider"], clients, "main-provider", explicit)
		if err != nil {
			t.Fatal(err)
		}
		for _, role := range []Role{RoleMain, RolePreflight, RoleReranker, RoleSummariser, RoleReview} {
			label := rm.LabelFor(role)
			if byLabel[label] != rm.For(role) {
				t.Errorf("config %v: %s runs on the provider registered as %q, "+
					"but LabelFor says %q", explicit, role, "<other>", label)
			}
		}
	}
}
