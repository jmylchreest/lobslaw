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

// JoinCluster asks each seed in turn to add this node as a voter.
// On success the leader has accepted us into the raft configuration —
// the local raft will see itself in the next replicated config and
// start participating. Returns nil on first success. If a seed
// answers but isn't leader it returns the leader address and we
// retry there. ctx caps total time across all seeds + redirects.
func (c *Client) JoinCluster(ctx context.Context, seeds []string, perDialTimeout time.Duration) error {
	if len(seeds) == 0 {
		return fmt.Errorf("no seeds configured for join")
	}
	if perDialTimeout <= 0 {
		perDialTimeout = 5 * time.Second
	}
	expanded := ExpandSeeds(ctx, seeds, c.resolver, c.logger)
	if len(expanded) == 0 {
		return fmt.Errorf("no seed addresses after expansion")
	}

	tried := map[string]bool{}
	queue := append([]string{}, expanded...)
	var lastErr error
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		addr := queue[0]
		queue = queue[1:]
		if tried[addr] {
			continue
		}
		tried[addr] = true

		leaderAddr, err := c.askJoin(ctx, addr, perDialTimeout)
		if err == nil {
			logging.From(ctx).Info("cluster join accepted", "via", addr)
			return nil
		}
		lastErr = err
		if leaderAddr != "" && !tried[leaderAddr] {
			logging.From(ctx).Info("cluster join: redirected to leader", "from", addr, "to", leaderAddr)
			queue = append(queue, leaderAddr)
			continue
		}
		logging.From(ctx).Warn("cluster join attempt failed", "addr", addr, "err", err)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no seed accepted join")
	}
	return lastErr
}

// askJoin dials a single peer and calls AddMember. Returns the
// leader address (so the caller can redirect) when the peer isn't
// leader; returns ("", nil) on success; ("", err) on hard failure.
func (c *Client) askJoin(ctx context.Context, addr string, perDialTimeout time.Duration) (string, error) {
	dialCtx, cancel := context.WithTimeout(ctx, perDialTimeout)
	defer cancel()

	conn, err := c.dial(dialCtx, addr)
	if err != nil {
		return "", fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	client := lobslawv1.NewNodeServiceClient(conn)
	resp, err := client.AddMember(dialCtx, &lobslawv1.AddMemberRequest{
		NodeId:  string(c.local.ID),
		Address: c.local.Address,
		Voter:   true,
	})
	if err != nil {
		return "", fmt.Errorf("AddMember: %w", err)
	}
	if resp.Accepted {
		return "", nil
	}
	return resp.LeaderAddress, fmt.Errorf("rejected by %s (leader=%q)", addr, resp.LeaderAddress)
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
