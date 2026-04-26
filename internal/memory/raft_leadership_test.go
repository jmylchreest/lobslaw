package memory

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/jmylchreest/lobslaw/pkg/crypto"
)

// TestSetLeadershipCallbackSeedsCurrentState verifies that wiring the
// callback after the node already became leader still surfaces the
// current state — the bug was that a leadership transition that
// happened before SetLeadershipCallback was called would be lost.
func TestSetLeadershipCallbackSeedsCurrentState(t *testing.T) {
	t.Parallel()
	node := newSingleNodeRaft(t)
	if err := node.WaitForLeader(5 * time.Second); err != nil {
		t.Fatal(err)
	}

	var fired atomic.Int32
	var lastSeen atomic.Bool
	node.SetLeadershipCallback(func(isLeader bool) {
		fired.Add(1)
		lastSeen.Store(isLeader)
	})

	if got := fired.Load(); got < 1 {
		t.Errorf("callback did not seed on registration; fired=%d", got)
	}
	if !lastSeen.Load() {
		t.Error("seeded value = false; expected true since node is the leader")
	}
}

// TestPublishLeadershipReconciles checks that publishLeadership reads
// state fresh and forwards the live value, regardless of any prior
// callback emissions. The reconciliation tick relies on this being
// correct.
func TestPublishLeadershipReconciles(t *testing.T) {
	t.Parallel()
	node := newSingleNodeRaft(t)
	if err := node.WaitForLeader(5 * time.Second); err != nil {
		t.Fatal(err)
	}

	var seen atomic.Bool
	node.SetLeadershipCallback(func(isLeader bool) { seen.Store(isLeader) })

	seen.Store(false) // pretend a stale value
	node.publishLeadership()
	if !seen.Load() {
		t.Error("publishLeadership did not refresh callback to live state")
	}
}

func newSingleNodeRaft(t *testing.T) *RaftNode {
	t.Helper()
	dataDir := t.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dataDir, "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	fsm := NewFSM(store)

	_, inmem := raft.NewInmemTransport(raft.ServerAddress("solo"))
	node, err := NewRaft(RaftConfig{
		NodeID:    "solo",
		LocalAddr: "solo",
		DataDir:   dataDir,
		Bootstrap: true,
		Transport: inmem,
	}, fsm)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = node.Shutdown()
		_ = store.Close()
	})
	return node
}
