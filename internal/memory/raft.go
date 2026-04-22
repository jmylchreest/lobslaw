package memory

import (
	"fmt"
	"os"
	"path/filepath"
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
	// Bootstrap controls whether this node should bootstrap a new
	// cluster as a single voter. Set true on first start of a new
	// cluster; false when joining an existing one.
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

	raftCfg := raft.DefaultConfig()
	raftCfg.LocalID = raft.ServerID(cfg.NodeID)
	raftCfg.HeartbeatTimeout = nonZeroDur(cfg.HeartbeatTimeout, 500*time.Millisecond)
	raftCfg.ElectionTimeout = nonZeroDur(cfg.ElectionTimeout, 500*time.Millisecond)
	raftCfg.LeaderLeaseTimeout = nonZeroDur(cfg.LeaderLeaseTimeout, 250*time.Millisecond)
	raftCfg.CommitTimeout = nonZeroDur(cfg.CommitTimeout, 50*time.Millisecond)

	r, err := raft.NewRaft(raftCfg, fsm, boltStore, boltStore, snapStore, cfg.Transport)
	if err != nil {
		_ = boltStore.Close()
		return nil, fmt.Errorf("construct raft: %w", err)
	}

	if cfg.Bootstrap {
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

	return &RaftNode{
		Raft:      r,
		transport: cfg.Transport,
		logStore:  boltStore,
		snapStore: snapStore,
		fsm:       fsm,
		dataDir:   cfg.DataDir,
	}, nil
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
	if err := n.Raft.Shutdown().Error(); err != nil {
		return fmt.Errorf("raft shutdown: %w", err)
	}
	if err := n.logStore.Close(); err != nil {
		return fmt.Errorf("raft.db close: %w", err)
	}
	return nil
}
