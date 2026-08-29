package egress

import "testing"

// The role fails closed: no declared callback address means no role,
// so a POST to an undeclared host is refused by the proxy rather than
// by nothing at all.
func TestCallbackRoleFailsClosed(t *testing.T) {
	rules := Build(ACLInputs{})
	if hosts, ok := rules.Roles["gateway/callback"]; ok {
		t.Errorf("no declared callbacks should mean no role, got %v", hosts)
	}
}

func TestCallbackRoleCarriesDeclaredHosts(t *testing.T) {
	rules := Build(ACLInputs{CallbackHosts: []string{"ops.example", "hooks.internal"}})
	got := rules.Roles["gateway/callback"]
	if len(got) != 2 || got[0] != "ops.example" {
		t.Errorf("gateway/callback = %v", got)
	}
}
