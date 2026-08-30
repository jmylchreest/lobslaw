package egress

import (
	"context"
	"net"
	"net/http"
)

// noopProvider returns vanilla http.Clients with no proxy and no
// role enforcement. Used in tests + for the brief boot window
// before the smokescreen provider is wired. NEVER used in prod
// after node.New returns — wire_egress.go installs the real one.
//
// Returning a working client even pre-wireup is deliberate: callers
// can grab egress.For(...) at struct-initialisation time without
// caring whether the provider has been swapped in yet, as long as
// they don't actually issue requests until after node boot.
type noopProvider struct {
	shared *http.Client
}

func newNoopProvider() *noopProvider {
	return &noopProvider{
		shared: &http.Client{Timeout: DefaultTimeout},
	}
}

func (p *noopProvider) For(role string) Client {
	return &noopClient{client: p.shared, role: role}
}

type noopClient struct {
	client *http.Client
	role   string
}

func (c *noopClient) HTTPClient() *http.Client { return c.client }
func (c *noopClient) Role() string             { return c.role }

// DialContext dials straight out. The noop provider is the "no egress
// filter configured" mode, so there is no proxy to tunnel through and
// nothing to enforce — same posture as its HTTPClient, which carries
// no proxy either.
func (c *noopClient) DialContext(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}
