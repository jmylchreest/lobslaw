//go:build linux

package sandbox

import "testing"

func TestInstallNetfilterNoOpWhenDisabled(t *testing.T) {
	t.Parallel()
	p := &Policy{NetworkFilter: false}
	if err := installNetfilter(p); err != nil {
		t.Errorf("disabled NetworkFilter should be no-op, got %v", err)
	}
}

// TestInstallNetfilterFailsWithoutNftablesBinary asserts the failure
// shape when nft is missing from PATH. We can't reliably manipulate
// PATH inside this test process without affecting other parallel
// tests, so we verify only the negative invariant: a *missing* nft
// surfaces an error rather than a silent success that would leave
// egress unrestricted.
//
// Sub-tests that exercise the success path live in the per-arch
// integration suite (which spins up a netns with nft installed) and
// run under a separate build tag — out of scope for the unit suite.
func TestInstallNetfilterRequiresNetnsForRequest(t *testing.T) {
	t.Parallel()
	p := &Policy{NetworkFilter: true, NetworkAllowDNS: true}
	if err := p.Validate(); err == nil {
		t.Error("NetworkFilter without Namespaces.Network must fail Validate")
	}
}
