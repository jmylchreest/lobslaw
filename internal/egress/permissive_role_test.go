package egress

import (
	"testing"

	smokeacl "github.com/stripe/smokescreen/pkg/smokescreen/acl/v1"
)

// fetch_url is documented as permissive by default, and a live node
// refused metoffice.gov.uk on it. The permissive rule renders as a
// single "*" glob; this asks whether smokescreen's matcher accepts
// that form at all.
func TestPermissiveRoleActuallyAllowsAHost(t *testing.T) {
	rules := Build(ACLInputs{})
	if !rules.Permissive["fetch_url"] {
		t.Fatal("fetch_url is not permissive by default")
	}
	acl := buildSmokescreenACL(rules)
	rule, ok := acl.Rules["fetch_url"]
	if !ok {
		t.Fatal("no fetch_url rule in the ACL")
	}
	t.Logf("fetch_url globs = %v policy=%v", rule.DomainGlobs, rule.Policy)
	dec, err := acl.Decide("fetch_url", "www.metoffice.gov.uk")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	t.Logf("decision = %+v", dec)
	if dec.Result != smokeacl.Allow {
		t.Errorf("permissive fetch_url did not allow a public host: %+v", dec)
	}
}

// The other half: an operator who declares an allowlist must still
// get one. A permissive default is only safe if opting out of it
// works.
func TestAnExplicitAllowlistStillRestricts(t *testing.T) {
	t.Parallel()
	rules := Build(ACLInputs{FetchURLAllowHosts: []string{"api.example.com"}})
	if rules.Permissive["fetch_url"] {
		t.Fatal("an explicit allowlist left the role permissive")
	}
	acl := buildSmokescreenACL(rules)
	allowed, err := acl.Decide("fetch_url", "api.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if allowed.Result != smokeacl.Allow {
		t.Errorf("the declared host was denied: %+v", allowed)
	}
	denied, err := acl.Decide("fetch_url", "evil.example.net")
	if err != nil {
		t.Fatal(err)
	}
	if denied.Result == smokeacl.Allow {
		t.Errorf("a host outside the allowlist was allowed: %+v", denied)
	}
}

// Private addresses stay refused on a permissive role — that check is
// smokescreen's own and is the part that actually matters.
func TestPermissiveDoesNotMeanPrivateAddresses(t *testing.T) {
	t.Parallel()
	rules := Build(ACLInputs{})
	if !rules.Permissive["fetch_url"] {
		t.Fatal("fetch_url is not permissive by default")
	}
	// The ACL allows by name; the private-IP refusal happens at
	// connect time in smokescreen's own resolver, which this test
	// documents rather than re-implements.
	if got := buildSmokescreenACL(rules).Rules["fetch_url"].Policy; got != smokeacl.Open {
		t.Errorf("policy = %v, want Open", got)
	}
}
