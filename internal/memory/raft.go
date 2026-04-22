package memory

import (
	"fmt"
	"net"
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
	// DataDir is where raft.db + snapshots/ live.
	DataDir string
	// Bootstrap controls whether this node should bootstrap a new
	// cluster as a single voter. Set true on first start of a new
	// cluster; false when joining an existing one.
	Bootstrap bool
	// InMemoryTransport selects the in-process transport (for tests
	// and Phase 2.3 pre-gRPC-transport scaffolding). Phase 2.4 swaps
	// in the real gRPC-backed transport.
	InMemoryTransport bool
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

	var transport raft.Transport
	var localAddr raft.ServerAddress
	if cfg.InMemoryTransport {
		localAddr = raft.ServerAddress(cfg.NodeID)
		_, transport = raft.NewInmemTransport(localAddr)
	} else {
		// Placeholder — Phase 2.4 wires the gRPC transport here.
		_ = net.Addr(nil)
		return nil, fmt.Errorf("non-in-memory transport not yet implemented (Phase 2.4)")
	}

	raftCfg := raft.DefaultConfig()
	raftCfg.LocalID = raft.ServerID(cfg.NodeID)
	// Tune down heartbeats/timeouts for test environments; production
	// deployments can override via environment or config later.
	raftCfg.HeartbeatTimeout = 500 * time.Millisecond
	raftCfg.ElectionTimeout = 500 * time.Millisecond
	raftCfg.LeaderLeaseTimeout = 250 * time.Millisecond
	raftCfg.CommitTimeout = 50 * time.Millisecond

	r, err := raft.NewRaft(raftCfg, fsm, boltStore, boltStore, snapStore, transport)
	if err != nil {
		_ = boltStore.Close()
		return nil, fmt.Errorf("construct raft: %w", err)
	}

	if cfg.Bootstrap {
		future := r.BootstrapCluster(raft.Configuration{
			Servers: []raft.Server{
				{ID: raft.ServerID(cfg.NodeID), Address: localAddr, Suffrage: raft.Voter},
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
		transport: transport,
		logStore:  boltStore,
		snapStore: snapStore,
		fsm:       fsm,
		dataDir:   cfg.DataDir,
	}, nil
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
