package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"

	"github.com/jmylchreest/lobslaw/internal/logging"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Dialer constructs a gRPC client connection to a peer. Implementations
// typically wrap grpc.NewClient with the cluster's mTLS credentials.
type Dialer func(ctx context.Context, addr string) (*grpc.ClientConn, error)

// Client dials seed nodes, registers this node, and folds each seed's
// peer list into the local registry. Used during node startup.
type Client struct {
	local    types.NodeInfo
	registry *Registry
	dial     Dialer
	resolver Resolver
	logger   *slog.Logger
}

// NewClient constructs a Client that writes learned peers into
// registry and dials seeds via dialer. Seed entries may be plain
// host:port or prefixed (srv:..., dns:...) — see ExpandSeeds.
func NewClient(local types.NodeInfo, registry *Registry, dial Dialer, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		local:    local,
		registry: registry,
		dial:     dial,
		resolver: DefaultResolver,
		logger:   logger,
	}
}

// SetResolver overrides the DNS resolver used to expand srv:/dns:
// seeds. Primarily for tests that inject a fake.
func (c *Client) SetResolver(r Resolver) { c.resolver = r }

// DialSeeds attempts to register with each seed in turn, folding the
// peers they know about into the local registry. Partial success is
// OK — a single reachable seed is enough to bootstrap. Returns the
// list of seed addresses that failed, and nil error unless every
// seed failed.
func (c *Client) DialSeeds(ctx context.Context, seeds []string, perDialTimeout time.Duration) (failed []string, err error) {
	if len(seeds) == 0 {
		return nil, nil
	}
	if perDialTimeout <= 0 {
		perDialTimeout = 5 * time.Second
	}

	// Expand srv:/dns: prefixed entries via DNS. Plain host:port
	// entries pass through unchanged.
	expanded := ExpandSeeds(ctx, seeds, c.resolver, c.logger)
	if len(expanded) == 0 {
		return seeds, fmt.Errorf("no seed addresses after expansion (check srv:/dns: resolvability)")
	}

	var succeeded int
	for _, addr := range expanded {
		if err := c.dialOne(ctx, addr, perDialTimeout); err != nil {
			logging.From(ctx).Warn("seed dial failed", "addr", addr, "err", err)
			failed = append(failed, addr)
			continue
		}
		succeeded++
	}
	if succeeded == 0 {
		return failed, fmt.Errorf("all %d seed dial attempts failed", len(seeds))
	}
	return failed, nil
}

// dialOne handles a single seed: dial, register, fetch peers, fold
// them into the registry. Connection is closed on return.
func (c *Client) dialOne(ctx context.Context, addr string, perDialTimeout time.Duration) error {
	dialCtx, cancel := context.WithTimeout(ctx, perDialTimeout)
	defer cancel()

	conn, err := c.dial(dialCtx, addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	client := lobslawv1.NewNodeServiceClient(conn)

	// Register ourselves with the seed.
	regResp, err := client.Register(dialCtx, &lobslawv1.RegisterRequest{
		Node: toProto(c.local),
	})
	if err != nil {
		return fmt.Errorf("Register: %w", err)
	}
	if !regResp.Accepted {
		return fmt.Errorf("Register rejected: %s", regResp.Reason)
	}

	// Ask the seed for its peer list and fold into ours.
	peersResp, err := client.GetPeers(dialCtx, &lobslawv1.GetPeersRequest{})
	if err != nil {
		return fmt.Errorf("GetPeers: %w", err)
	}
	for _, p := range peersResp.Peers {
		info := fromProto(p)
		if info.ID == "" || info.ID == c.local.ID {
			continue
		}
		c.registry.Register(info)
	}
	logging.From(ctx).Info("seed exchange complete",
		"addr", addr,
		"learned_peers", len(peersResp.Peers),
	)
	return nil
}
