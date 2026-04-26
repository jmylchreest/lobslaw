package memory

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
)

// RaftConfig holds the parameters for constructing a Raft node.
type RaftConfig struct {
	// NodeID is the raft.ServerID — must be stable across restarts.
	NodeID string
	// LocalAddr is this node's gRPC listen address as peers see it.
	// For the in-memory transport, pass a label matching NodeID.
	LocalAddr raft.ServerAddress
	// DataDir is where raft.db + snapshots/ live.
	DataDir string
	// Bootstrap, when true AND no prior raft state exists on disk,
	// makes NewRaft form a fresh single-voter cluster from this
	// node. With existing state present, this flag is ignored — the
	// persisted configuration wins. The orchestration layer
	// (internal/node) decides whether to set this based on the join
	// flow's outcome; tests pass true to skip the join dance.
	Bootstrap bool
	// Transport is the raft.Transport implementation. Phase 2.4
	// injects pkg/rafttransport's gRPC transport; tests can pass
	// raft.NewInmemTransport(...) for single-process verification.
	Transport raft.Transport
	// Timeouts override Raft's heartbeat/election tuning. Zero values
	// use sensible defaults tuned for test stability.
	HeartbeatTimeout   time.Duration
	ElectionTimeout    time.Duration
	LeaderLeaseTimeout time.Duration
	CommitTimeout      time.Duration
	// Logger receives raft's structured chatter (via an hclog adapter)
	// and the leadership-change watcher's INFO logs. Nil → slog.Default.
	Logger *slog.Logger
}

// RaftNode wraps a *raft.Raft with its dependencies. Callers do not
// operate on the inner fields directly — use the Apply/Shutdown
// methods.
type RaftNode struct {
	Raft      *raft.Raft
	transport raft.Transport
	logStore  *raftboltdb.BoltStore
	snapStore raft.SnapshotStore
	fsm       *FSM
	dataDir   string
	log       *slog.Logger
	stopWatch chan struct{}
	watchOnce sync.Once
	watchWG   sync.WaitGroup

	// nodeID, localAddr are kept so BootstrapSelf can install a
	// single-voter config without re-threading the values.
	nodeID    raft.ServerID
	localAddr raft.ServerAddress
	// hadState records the result of raft.HasExistingState as seen
	// at NewRaft time — used by the boot orchestration to choose
	// between resume / join / bootstrap.
	hadState bool

	// onLeadership is invoked from the state-watcher goroutine
	// whenever raft leadership flips on this node. internal/node
	// wires this to a singleton.LeaderGate so leader-gated workloads
	// (e.g. the telegram long-poller) start/stop correctly. nil-safe.
	onLeadership func(bool)
}

// NewRaft constructs a Raft node bound to fsm. For Phase 2.3 only the
// in-memory transport is wired; Phase 2.4 will introduce a gRPC
// transport factory that replaces it.
func NewRaft(cfg RaftConfig, fsm *FSM) (*RaftNode, error) {
	if cfg.NodeID == "" {
		return nil, fmt.Errorf("RaftConfig: NodeID required")
	}
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("RaftConfig: DataDir required")
	}
	if cfg.Transport == nil {
		return nil, fmt.Errorf("RaftConfig: Transport required")
	}
	if cfg.LocalAddr == "" {
		return nil, fmt.Errorf("RaftConfig: LocalAddr required")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	raftDB := filepath.Join(cfg.DataDir, "raft.db")
	boltStore, err := raftboltdb.New(raftboltdb.Options{Path: raftDB})
	if err != nil {
		return nil, fmt.Errorf("open raft.db: %w", err)
	}

	snapDir := filepath.Join(cfg.DataDir, "snapshots")
	snapStore, err := raft.NewFileSnapshotStore(snapDir, 2, os.Stderr)
	if err != nil {
		_ = boltStore.Close()
		return nil, fmt.Errorf("create snapshot store: %w", err)
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	raftCfg := raft.DefaultConfig()
	raftCfg.LocalID = raft.ServerID(cfg.NodeID)
	raftCfg.Logger = newHCLogAdapter(logger, "raft")
	raftCfg.HeartbeatTimeout = nonZeroDur(cfg.HeartbeatTimeout, 500*time.Millisecond)
	raftCfg.ElectionTimeout = nonZeroDur(cfg.ElectionTimeout, 500*time.Millisecond)
	raftCfg.LeaderLeaseTimeout = nonZeroDur(cfg.LeaderLeaseTimeout, 250*time.Millisecond)
	raftCfg.CommitTimeout = nonZeroDur(cfg.CommitTimeout, 50*time.Millisecond)

	hadState, err := raft.HasExistingState(boltStore, boltStore, snapStore)
	if err != nil {
		_ = boltStore.Close()
		return nil, fmt.Errorf("check raft state: %w", err)
	}

	r, err := raft.NewRaft(raftCfg, fsm, boltStore, boltStore, snapStore, cfg.Transport)
	if err != nil {
		_ = boltStore.Close()
		return nil, fmt.Errorf("construct raft: %w", err)
	}

	// Bootstrap is honoured only on a virgin data dir. If state exists,
	// the persisted configuration wins — re-bootstrapping would either
	// be a no-op (raft.ErrCantBootstrap) or, worse, break a working
	// multi-node cluster by trying to install a single-voter config.
	if cfg.Bootstrap && !hadState {
		future := r.BootstrapCluster(raft.Configuration{
			Servers: []raft.Server{
				{ID: raft.ServerID(cfg.NodeID), Address: cfg.LocalAddr, Suffrage: raft.Voter},
			},
		})
		if err := future.Error(); err != nil && err != raft.ErrCantBootstrap {
			_ = r.Shutdown().Error()
			_ = boltStore.Close()
			return nil, fmt.Errorf("bootstrap cluster: %w", err)
		}
	}

	node := &RaftNode{
		Raft:      r,
		transport: cfg.Transport,
		logStore:  boltStore,
		snapStore: snapStore,
		fsm:       fsm,
		dataDir:   cfg.DataDir,
		log:       logger,
		stopWatch: make(chan struct{}),
		nodeID:    raft.ServerID(cfg.NodeID),
		localAddr: cfg.LocalAddr,
		hadState:  hadState,
	}
	node.startStateWatch()
	return node, nil
}

// HadStateOnBoot reports whether NewRaft loaded a non-empty raft.db
// at construction time. The orchestration layer uses this to
// distinguish "virgin node — try to join then bootstrap" from
// "existing node — resume and verify membership".
func (n *RaftNode) HadStateOnBoot() bool { return n.hadState }

// IsInConfiguration reports whether this node's LocalID appears in
// the cluster's currently committed configuration. False on a virgin
// node (empty config); also false if a stale data dir was carried
// over from a previous identity (the "lptjmfw" orphan case).
func (n *RaftNode) IsInConfiguration() bool {
	cfgFuture := n.Raft.GetConfiguration()
	if err := cfgFuture.Error(); err != nil {
		return false
	}
	for _, s := range cfgFuture.Configuration().Servers {
		if s.ID == n.nodeID {
			return true
		}
	}
	return false
}

// ConfigurationServers returns the raft servers currently in the
// committed configuration. Useful for diagnostics + the orphan
// fail-fast message ("you're not in the config; the config has X").
func (n *RaftNode) ConfigurationServers() []raft.Server {
	cfgFuture := n.Raft.GetConfiguration()
	if err := cfgFuture.Error(); err != nil {
		return nil
	}
	return cfgFuture.Configuration().Servers
}

// BootstrapSelf installs a single-voter configuration containing
// only this node. Safe to call on a node that already has state —
// raft returns ErrCantBootstrap which we swallow. Used by the boot
// orchestration when no existing cluster could be reached.
func (n *RaftNode) BootstrapSelf() error {
	future := n.Raft.BootstrapCluster(raft.Configuration{
		Servers: []raft.Server{
			{ID: n.nodeID, Address: n.localAddr, Suffrage: raft.Voter},
		},
	})
	if err := future.Error(); err != nil && err != raft.ErrCantBootstrap {
		return fmt.Errorf("bootstrap cluster: %w", err)
	}
	return nil
}

// WaitForConfigInclusion blocks until this node's LocalID appears in
// the committed configuration (i.e. the cluster leader has processed
// our AddVoter and replicated the new config to us), or until ctx is
// cancelled. Used after the join flow to confirm the leader actually
// added us before declaring boot success.
func (n *RaftNode) WaitForConfigInclusion(ctx context.Context, poll time.Duration) error {
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		if n.IsInConfiguration() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// startStateWatch spawns a goroutine that emits INFO logs whenever
// raft leadership flips or the cluster configuration changes. This
// is the bit operators actually want to see at a glance — raft's own
// logger covers the rest at debug.
//
// Leadership transitions reach the onLeadership callback through
// THREE paths, and any of them is sufficient on its own:
//
//  1. raft.LeaderCh() — primary signal, but a buffered size-1 chan;
//     rapid Leader→Follower→Leader bounces can drop a transition.
//  2. RaftState observer events — every internal state change.
//  3. A 1s reconciliation tick that re-publishes the current state.
//
// publishLeadership() is idempotent (LeaderGate.Publish no-ops when
// the value matches), so over-firing is cheap and double-publishing
// from different paths is harmless. The reconciliation tick is the
// safety net for cases where both observer + leaderCh miss an edge
// (e.g. demotion during raft shutdown of the previous term).
func (n *RaftNode) startStateWatch() {
	n.watchOnce.Do(func() {
		n.watchWG.Add(1)
		go func() {
			defer n.watchWG.Done()
			leaderCh := n.Raft.LeaderCh()
			obsCh := make(chan raft.Observation, 16)
			obs := raft.NewObserver(obsCh, false, func(o *raft.Observation) bool {
				switch o.Data.(type) {
				case raft.LeaderObservation, raft.PeerObservation, raft.RaftState:
					return true
				}
				return false
			})
			n.Raft.RegisterObserver(obs)
			defer n.Raft.DeregisterObserver(obs)

			// Periodic DEBUG snapshot of cluster state.
			snapshot := time.NewTicker(30 * time.Second)
			defer snapshot.Stop()

			// Reconciliation tick — the safety net described above.
			// 1s is fast enough that singleton workloads (telegram
			// poller, scheduler claims) catch up well within a
			// human-noticeable window after any missed edge.
			reconcile := time.NewTicker(1 * time.Second)
			defer reconcile.Stop()

			for {
				select {
				case <-n.stopWatch:
					return
				case <-snapshot.C:
					n.logClusterSnapshot()
				case <-reconcile.C:
					n.publishLeadership()
				case isLeader := <-leaderCh:
					addr, id := n.Raft.LeaderWithID()
					n.log.Info("raft leadership changed",
						"is_leader", isLeader,
						"leader_id", string(id),
						"leader_addr", string(addr),
						"term", n.Raft.CurrentTerm())
					n.publishLeadership()
				case ev := <-obsCh:
					switch d := ev.Data.(type) {
					case raft.LeaderObservation:
						n.log.Info("raft leader observed",
							"leader_id", string(d.LeaderID),
							"leader_addr", string(d.LeaderAddr),
							"term", n.Raft.CurrentTerm())
					case raft.PeerObservation:
						n.log.Info("raft peer changed",
							"peer_id", string(d.Peer.ID),
							"peer_addr", string(d.Peer.Address),
							"removed", d.Removed,
							"suffrage", d.Peer.Suffrage.String())
					case raft.RaftState:
						n.log.Info("raft state changed", "state", d.String())
					}
					// State / leader / peer change → re-publish.
					// LeaderObservation in particular flags a
					// leader-loss-with-no-immediate-replacement that
					// the buffered LeaderCh can swallow.
					n.publishLeadership()
				}
			}
		}()
	})
}

// publishLeadership emits the current leadership state to whatever
// callback was last registered via SetLeadershipCallback. Always
// reads State() fresh — never trusts a cached value — so it's the
// authoritative reconcile point. Safe to call from anywhere; no-op
// when no callback is wired.
func (n *RaftNode) publishLeadership() {
	cb := n.onLeadership
	if cb == nil {
		return
	}
	cb(n.Raft.State() == raft.Leader)
}

// logClusterSnapshot emits a single DEBUG line summarising current
// raft state — leader, term, log indexes, and the per-server
// configuration with suffrages. The configuration section is what
// usually surfaces split-brain bootstraps: each node listing only
// itself means they bootstrapped independently and never joined.
func (n *RaftNode) logClusterSnapshot() {
	if !n.log.Enabled(nil, slog.LevelDebug) {
		return
	}
	stats := n.Raft.Stats()
	leaderAddr, leaderID := n.Raft.LeaderWithID()

	servers := []string{}
	cfgFuture := n.Raft.GetConfiguration()
	if err := cfgFuture.Error(); err == nil {
		for _, s := range cfgFuture.Configuration().Servers {
			servers = append(servers, fmt.Sprintf("%s@%s(%s)", s.ID, s.Address, s.Suffrage))
		}
	}

	n.log.Debug("raft cluster snapshot",
		"local_id", n.Raft.String(),
		"state", stats["state"],
		"term", stats["term"],
		"leader_id", string(leaderID),
		"leader_addr", string(leaderAddr),
		"last_log_index", stats["last_log_index"],
		"applied_index", stats["applied_index"],
		"commit_index", stats["commit_index"],
		"num_peers", stats["num_peers"],
		"last_contact", stats["last_contact"],
		"servers", servers,
	)
}

func nonZeroDur(v, fallback time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return fallback
}

// AddVoter requests that addr (a future cluster member at the given
// raft.ServerAddress) join as a voting member. Must be called on the
// leader.
func (n *RaftNode) AddVoter(id raft.ServerID, addr raft.ServerAddress) error {
	future := n.Raft.AddVoter(id, addr, 0, 10*time.Second)
	return future.Error()
}

// FSM returns the finite-state machine this Raft node is driving.
// Exposed so read-path code (minimal PolicyService, tests) can access
// the backing Store without going through raft.Apply.
func (n *RaftNode) FSM() *FSM {
	return n.fsm
}

// IsLeader reports whether this node currently holds Raft leadership.
// Non-leader writes through raft.Apply return ErrNotLeader; callers
// that want to be polite check IsLeader first and redirect.
func (n *RaftNode) IsLeader() bool {
	return n.Raft.State() == raft.Leader
}

// SetLeadershipCallback registers a function called whenever raft
// leadership flips on this node. internal/node uses this to feed a
// singleton.LeaderGate. Pass nil to clear. Replaces any previous
// callback — only one consumer is expected because the gate itself
// fans out to subscribers.
//
// On non-nil registration, the current leadership state is published
// immediately. Without that seed, a leadership transition that
// happened between NewRaft (which starts the state-watcher) and the
// caller wiring this callback would be lost — the gate would carry
// false until the next genuine flip.
func (n *RaftNode) SetLeadershipCallback(cb func(bool)) {
	n.onLeadership = cb
	if cb != nil {
		cb(n.Raft.State() == raft.Leader)
	}
}

// LeaderAddress returns the current leader's advertised address. Empty
// during an election.
func (n *RaftNode) LeaderAddress() raft.ServerAddress {
	addr, _ := n.Raft.LeaderWithID()
	return addr
}

// Apply serialises data through Raft consensus. Returns the FSM's
// Apply return value on success.
func (n *RaftNode) Apply(data []byte, timeout time.Duration) (any, error) {
	future := n.Raft.Apply(data, timeout)
	if err := future.Error(); err != nil {
		return nil, err
	}
	return future.Response(), nil
}

// WaitForLeader blocks until this node becomes the leader, or timeout.
// Used by single-node startup + tests.
func (n *RaftNode) WaitForLeader(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if n.Raft.State() == raft.Leader {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for leader (state=%s)", n.Raft.State())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Shutdown closes the Raft node and its log/snapshot backing.
func (n *RaftNode) Shutdown() error {
	close(n.stopWatch)
	n.watchWG.Wait()
	if err := n.Raft.Shutdown().Error(); err != nil {
		return fmt.Errorf("raft shutdown: %w", err)
	}
	if err := n.logStore.Close(); err != nil {
		return fmt.Errorf("raft.db close: %w", err)
	}
	return nil
}
