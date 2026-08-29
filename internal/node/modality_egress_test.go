package node

import (
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/egress"
)

// Modality drivers were the only outbound path not routed through
// smokescreen. What makes the routing real is that the transport comes
// from the ACTIVE PROVIDER for that role — under smokescreen it carries
// the proxy and the role header, and the driver inherits both by using
// this client rather than one of its own.
//
// Asserted against the provider rather than against a concrete proxy
// transport because a unit test has no smokescreen running: egress.For
// returns the noop provider here, and pinning the noop's shape would
// test the fixture instead of the wiring.
func TestModalityEgressClientComesFromTheActiveProvider(t *testing.T) {
	role := "llm/qwen-image"
	want := egress.For(role).HTTPClient()
	got := modalityEgressClient("qwen-image")
	if got == nil {
		t.Fatal("nil client")
	}
	if got == want {
		t.Fatal("the shared client was returned directly; setting a timeout on it would affect every caller of this role")
	}
	if got.Transport != want.Transport {
		t.Error("transport is not the provider's; the request would bypass the proxy")
	}
}

// egress.For hands out a SHARED client per role with a 30s timeout —
// right for a chat round trip, wrong for image generation, which the
// image driver itself allows three minutes for. Passing that client
// unmodified turned "route this through the proxy" into a timeout on
// every picture.
func TestModalityEgressClientDoesNotTightenDriverDeadlines(t *testing.T) {
	c := modalityEgressClient("qwen-image")
	if c.Timeout <= egress.DefaultTimeout {
		t.Errorf("timeout %v does not exceed egress.DefaultTimeout %v", c.Timeout, egress.DefaultTimeout)
	}
	// Looser than the longest driver default in the tree (3 minutes,
	// image generation) so routing cannot shorten a deadline a driver
	// already chose.
	if c.Timeout < 3*time.Minute {
		t.Errorf("timeout %v is shorter than the longest driver default", c.Timeout)
	}
}

// The provider hands out one client per role. Mutating it in place
// would change the deadline for every other caller of that role.
func TestModalityEgressClientIsCopied(t *testing.T) {
	shared := egress.For("llm/qwen-image").HTTPClient()
	before := shared.Timeout
	_ = modalityEgressClient("qwen-image")
	if shared.Timeout != before {
		t.Errorf("the shared client's timeout changed from %v to %v", before, shared.Timeout)
	}
}
