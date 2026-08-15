package memory

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Only the Raft leader may write, but a user's message lands on
// whichever gateway node they happened to reach, and that is
// uncorrelated with leadership. Before forwarding, a two-node cluster
// whose gateway was not the leader could not persist a single
// conversation turn: every write path returned "not the raft leader;
// retry at <addr>" and left the retry to a caller that had no way to
// perform it.
//
// Forwarding sends the marshalled LogEntry to the leader's Propose
// RPC over the cluster's existing mTLS connection. Deliberately not a
// retry loop around Apply: one hop to a known address, and leadership
// churn mid-forward surfaces as an error the caller can retry rather
// than as latency that hides a partition.

var (
	// ErrNoLeader means there is no leader to forward to — an
	// election is in progress. Distinct from ErrNotLeader because
	// the responses differ: wait and retry, versus talk to someone
	// else. Callers and logs need to tell them apart.
	ErrNoLeader = errors.New("raft: no leader elected")

	// ErrForwardUnavailable means a leader exists but this node
	// cannot forward to it — no dialer wired, or the RPC failed.
	// Separate from ErrNoLeader so a misconfigured node does not
	// look like an election that never ends.
	ErrForwardUnavailable = errors.New("raft: cannot forward to leader")
)

// LeaderDialer opens a connection to a peer's gRPC server. The Raft
// transport shares that server, so a raft.ServerAddress is dialable
// as-is — no separate address book.
type LeaderDialer func(ctx context.Context, addr string) (*grpc.ClientConn, error)

// leaderConns caches one connection per leader address. Leadership
// changes rarely, so this is a small map that mostly holds one entry;
// grpc.ClientConn is safe for concurrent use and reconnects
// internally, so entries stay valid across transient drops.
type leaderConns struct {
	mu    sync.Mutex
	dial  LeaderDialer
	conns map[string]*grpc.ClientConn
}

func (c *leaderConns) get(ctx context.Context, addr string) (*grpc.ClientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if conn, ok := c.conns[addr]; ok {
		return conn, nil
	}
	conn, err := c.dial(ctx, addr)
	if err != nil {
		return nil, err
	}
	if c.conns == nil {
		c.conns = make(map[string]*grpc.ClientConn)
	}
	c.conns[addr] = conn
	return conn, nil
}

func (c *leaderConns) closeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for addr, conn := range c.conns {
		_ = conn.Close()
		delete(c.conns, addr)
	}
}

// dialer returns the configured dialer, or nil.
func (c *leaderConns) dialer() LeaderDialer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dial
}

// SetLeaderDialer wires the forwarding path. Without it a follower
// write fails with ErrForwardUnavailable rather than silently
// pretending to have succeeded. Single-node deployments never call
// the dialer, since IsLeader is always true.
func (n *RaftNode) SetLeaderDialer(dial LeaderDialer) {
	n.fwd.mu.Lock()
	n.fwd.dial = dial
	n.fwd.mu.Unlock()
}

// ApplyOrForward applies data on this node when it is the leader, and
// otherwise forwards it to the leader for application.
//
// This is the write path for anything that must succeed regardless of
// which node received the request — sessions, prefs, credentials,
// memory. It is NOT for leader-gated background work: Dream, session
// pruning and the scheduler are singletons that should skip on a
// follower, not relocate their work to the leader. Those keep Apply
// and their own IsLeader guard.
//
// The returned error is ErrNoLeader during an election and
// ErrForwardUnavailable when a leader exists but is unreachable;
// both are retryable, and a caller that cannot distinguish them
// cannot log usefully.
func (n *RaftNode) ApplyOrForward(ctx context.Context, data []byte, timeout time.Duration) (any, error) {
	if n.IsLeader() {
		return n.Apply(data, timeout)
	}

	addr := string(n.LeaderAddress())
	if addr == "" {
		return nil, ErrNoLeader
	}
	if n.fwd.dialer() == nil {
		return nil, fmt.Errorf("%w: leader is %s but no dialer is wired", ErrForwardUnavailable, addr)
	}

	// The forward inherits the caller's deadline where there is one,
	// and otherwise gets the same budget the local apply would have
	// had. Without this an unreachable leader would hang a turn.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	conn, err := n.fwd.get(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("%w: dial %s: %w", ErrForwardUnavailable, addr, err)
	}
	if _, err := lobslawv1.NewNodeServiceClient(conn).Propose(ctx,
		&lobslawv1.ProposeRequest{Entry: data}); err != nil {
		// Includes the far side answering "not the leader" — it lost
		// leadership between our read of LeaderAddress and the call.
		// Retryable, and the caller re-reads the leader on the way
		// back through.
		return nil, fmt.Errorf("%w: propose to %s: %w", ErrForwardUnavailable, addr, err)
	}
	// Propose carries no FSM return value: the entry is applied on
	// the leader and replicated here. Callers that need the result
	// read it back from the local store after the apply lands.
	return nil, nil
}
