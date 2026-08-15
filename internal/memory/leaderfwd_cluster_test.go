package memory_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/pkg/crypto"

	"github.com/jmylchreest/lobslaw/internal/discovery"
	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/rafttransport"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Only the leader may write, but a user's message lands on whichever
// gateway node they reached. Before forwarding, a follower could not
// persist a conversation turn at all — every write returned "not the
// raft leader; retry at <addr>" to a caller with no way to retry
// there. These tests are the cluster-level proof that a write issued
// on a follower reaches the leader and replicates back.

// TestFollowerWriteReachesLeader is R0's first acceptance criterion:
// a three-node cluster where the writing node is a follower persists
// the write with no operator action.
func TestFollowerWriteReachesLeader(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-node integration in short mode")
	}
	c := newForwardingCluster(t)

	follower := c.follower(t)
	if follower.raft.IsLeader() {
		t.Fatal("picked the leader; the point is to write on a node that is not it")
	}

	// A session append is the write R0 exists for: it is issued by
	// whichever gateway node the user reached.
	sessions := memory.NewSessionService(follower.raft, follower.store, memory.SessionConfig{})
	rec, err := sessions.Append(context.Background(),
		memory.SessionRef{Channel: "telegram", ChannelID: "-100"},
		"turn-1",
		[]memory.TranscriptMessage{{Role: "user", Content: "written on a follower"}})
	if err != nil {
		t.Fatalf("follower could not persist a turn: %v", err)
	}
	if rec == nil || rec.Id == "" {
		t.Fatal("append returned no session record")
	}

	// The entry must exist on the leader's store — that is what
	// proves it went through Raft rather than being written locally.
	leader := c.leader(t)
	waitFor(t, 5*time.Second, func() bool {
		raw, err := leader.store.Get(memory.BucketSessions, rec.Id)
		return err == nil && len(raw) > 0
	}, "session never reached the leader's store")

	// And back to the follower that issued it, via replication.
	waitFor(t, 5*time.Second, func() bool {
		raw, err := follower.store.Get(memory.BucketSessions, rec.Id)
		return err == nil && len(raw) > 0
	}, "session never replicated back to the writing follower")
}

// TestForwardedWriteIsNotForwardedAgain pins the no-cycles property.
// Propose is leader-only by construction: a follower that receives one
// refuses rather than passing it on, so a forward is always one hop.
func TestForwardedWriteIsNotForwardedAgain(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-node integration in short mode")
	}
	c := newForwardingCluster(t)
	f1, f2 := c.twoFollowers(t)

	// Aim a Propose directly at a follower, which is what a second
	// hop would look like.
	conn, err := grpc.NewClient(f2.addr, grpc.WithTransportCredentials(clientCredsFor(t, c.certDir, f1.index)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	entry, err := marshalPolicyEntry("should-not-apply")
	if err != nil {
		t.Fatal(err)
	}
	_, err = lobslawv1.NewNodeServiceClient(conn).Propose(context.Background(),
		&lobslawv1.ProposeRequest{Entry: entry})
	if err == nil {
		t.Fatal("a follower accepted a Propose; it must refuse rather than forward on")
	}

	// It must also not have applied it locally, bypassing Raft.
	if raw, err := f2.store.Get(memory.BucketPolicyRules, "should-not-apply"); err == nil && len(raw) > 0 {
		t.Error("the follower applied the entry to its own store")
	}
}

// TestForwardDistinguishesNoLeaderFromUnreachable is R0's third
// acceptance criterion. The two are separately actionable — one says
// wait, the other says fix your wiring — so a caller that cannot tell
// them apart cannot log anything useful.
func TestForwardDistinguishesNoLeaderFromUnreachable(t *testing.T) {
	t.Parallel()
	// A node that never joins a cluster has no leader at all.
	_, trans := raft.NewInmemTransport("orphan")
	fsm := memory.NewFSM(newOrphanStore(t))
	r, err := memory.NewRaft(memory.RaftConfig{
		NodeID:    "orphan",
		LocalAddr: "orphan",
		DataDir:   t.TempDir(),
		Transport: trans,
	}, fsm)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Shutdown() }()

	entry, err := marshalPolicyEntry("x")
	if err != nil {
		t.Fatal(err)
	}

	// No leader elected: forwarding has no address to aim at.
	_, err = r.ApplyOrForward(context.Background(), entry, time.Second)
	if !errors.Is(err, memory.ErrNoLeader) {
		t.Errorf("got %v, want ErrNoLeader — an orphan node has no leader to forward to", err)
	}
	if errors.Is(err, memory.ErrForwardUnavailable) {
		t.Error("ErrNoLeader must not also match ErrForwardUnavailable; callers switch on them")
	}
}

// --- harness ------------------------------------------------------

type forwardingCluster struct {
	nodes   []*clusterNode
	certDir string
}

func (c *forwardingCluster) leader(t *testing.T) *clusterNode {
	t.Helper()
	for _, n := range c.nodes {
		if n.raft.IsLeader() {
			return n
		}
	}
	t.Fatal("no leader")
	return nil
}

func (c *forwardingCluster) follower(t *testing.T) *clusterNode {
	t.Helper()
	for _, n := range c.nodes {
		if !n.raft.IsLeader() {
			return n
		}
	}
	t.Fatal("no follower")
	return nil
}

func (c *forwardingCluster) twoFollowers(t *testing.T) (*clusterNode, *clusterNode) {
	t.Helper()
	var out []*clusterNode
	for _, n := range c.nodes {
		if !n.raft.IsLeader() {
			out = append(out, n)
		}
	}
	if len(out) < 2 {
		t.Fatalf("want 2 followers, got %d", len(out))
	}
	return out[0], out[1]
}

// newForwardingCluster is the raft_cluster_test harness plus the two
// things forwarding needs: a NodeService on each gRPC server so
// Propose is reachable, and a leader dialer on each RaftNode.
func newForwardingCluster(t *testing.T) *forwardingCluster {
	t.Helper()
	certDir, caCert, caKey, caCertPath := newClusterCA(t)

	const nodeCount = 3
	nodes := make([]*clusterNode, nodeCount)
	for i := range nodes {
		nodes[i] = newClusterNode(t, i, certDir, caCertPath, caCert, caKey)
	}
	t.Cleanup(func() {
		for _, n := range nodes {
			n.shutdown()
		}
	})

	// Transports and the NodeService must both register before Serve —
	// gRPC refuses RegisterService afterwards.
	for i, n := range nodes {
		rt, err := rafttransport.New(rafttransport.Config{
			LocalAddr: raft.ServerAddress(n.addr),
			DialOpts:  []grpc.DialOption{grpc.WithTransportCredentials(clientCredsFor(t, certDir, i))},
		})
		if err != nil {
			t.Fatalf("rafttransport.New for %s: %v", n.id, err)
		}
		rt.Register(n.server)
		n.transport = rt
	}

	// RaftNode before Serve: gRPC refuses RegisterService once a
	// server is serving, and registering NodeService needs the
	// RaftNode. NewRaft only constructs — it does not dial peers — so
	// there is nothing here that requires the listeners to be live.
	for _, n := range nodes {
		r, err := memory.NewRaft(memory.RaftConfig{
			NodeID:    n.id,
			LocalAddr: raft.ServerAddress(n.addr),
			DataDir:   filepath.Join(t.TempDir(), n.id),
			Transport: n.transport.RaftTransport(),
		}, n.fsm)
		if err != nil {
			t.Fatalf("NewRaft %s: %v", n.id, err)
		}
		n.raft = r

		lobslawv1.RegisterNodeServiceServer(n.server,
			discovery.NewService(discovery.NewRegistry(),
				types.NodeInfo{ID: types.NodeID(n.id), Address: n.addr}, nil, nil, r))

		// The Raft transport shares this server, so the address Raft
		// reports for the leader is dialable as-is.
		idx := n.index
		r.SetLeaderDialer(func(ctx context.Context, addr string) (*grpc.ClientConn, error) {
			return grpc.NewClient(addr, grpc.WithTransportCredentials(clientCredsFor(t, certDir, idx)))
		})
	}

	for _, n := range nodes {
		go func(n *clusterNode) { _ = n.server.Serve(n.listener) }(n)
	}

	bootstrapAndJoin(t, nodes)
	return &forwardingCluster{nodes: nodes, certDir: certDir}
}

func marshalPolicyEntry(id string) ([]byte, error) {
	return proto.Marshal(&lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT,
		Id: id,
		Payload: &lobslawv1.LogEntry_PolicyRule{
			PolicyRule: &lobslawv1.PolicyRule{
				Id: id, Subject: "*", Action: "*", Resource: "*", Effect: "deny",
			},
		},
	})
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}

// newOrphanStore is a bare encrypted store for a node that will never
// join a cluster.
func newOrphanStore(t *testing.T) *memory.Store {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(t.TempDir(), "orphan.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestLeaderLossSurfacesRetryableError is R0's second acceptance
// criterion: killing the leader mid-turn must surface something the
// caller can retry, not lose the message.
//
// The property is two-part and both halves matter. Every failure
// during the election is one of the retryable sentinels — never a
// generic error that a caller would treat as permanent and drop the
// turn — and once a new leader settles the same write succeeds
// against it, which is what "not lost" means.
func TestLeaderLossSurfacesRetryableError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-node integration in short mode")
	}
	c := newForwardingCluster(t)

	writer := c.follower(t)
	old := c.leader(t)
	sessions := memory.NewSessionService(writer.raft, writer.store, memory.SessionConfig{})
	ref := memory.SessionRef{Channel: "telegram", ChannelID: "-200"}

	// A write before the kill, so we know the path works at all.
	if _, err := sessions.Append(context.Background(), ref, "turn-before",
		[]memory.TranscriptMessage{{Role: "user", Content: "before"}}); err != nil {
		t.Fatalf("baseline append failed: %v", err)
	}

	// Kill the leader. The remaining two still hold quorum, so a new
	// one is elected — but not instantly.
	if err := old.raft.Shutdown(); err != nil {
		t.Fatalf("shutdown leader: %v", err)
	}
	old.server.Stop()

	var lastErr error
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		_, err := sessions.Append(context.Background(), ref, "turn-after",
			[]memory.TranscriptMessage{{Role: "user", Content: "after"}})
		if err == nil {
			return // re-established against the new leader
		}
		lastErr = err
		// Every intermediate failure must be retryable. A caller that
		// cannot recognise the error drops the user's message.
		if !errors.Is(err, memory.ErrNoLeader) &&
			!errors.Is(err, memory.ErrForwardUnavailable) &&
			!errors.Is(err, memory.ErrNotLeader) {
			t.Fatalf("non-retryable error during election: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("write never recovered after leader loss; last error: %v", lastErr)
}

// Shutdown is called explicitly by code that stops a node and again by
// deferred cleanup. The second call used to panic on an
// already-closed channel, which surfaced as a test crash rather than
// as the shutdown bug it was.
func TestRaftShutdownIsIdempotent(t *testing.T) {
	t.Parallel()
	_, trans := raft.NewInmemTransport("twice")
	r, err := memory.NewRaft(memory.RaftConfig{
		NodeID:    "twice",
		LocalAddr: "twice",
		DataDir:   t.TempDir(),
		Transport: trans,
	}, memory.NewFSM(newOrphanStore(t)))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Shutdown(); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}
	// Must not panic. An error is acceptable — the log store is
	// already closed — but a crash is not.
	_ = r.Shutdown()
}
