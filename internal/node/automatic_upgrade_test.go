package node

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/jmylchreest/lobslaw/internal/audit"
	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/internal/memory"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestAutomaticControllerActivatesWithAuditAfterStability(t *testing.T) {
	ctx := context.Background()
	store := crossOwnerTestStore(t)
	_, transport := raft.NewInmemTransport("auto-node")
	rn, err := memory.NewRaft(memory.RaftConfig{NodeID: "auto-node", LocalAddr: "auto-node", DataDir: t.TempDir(), Bootstrap: true, Transport: transport}, memory.NewFSM(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rn.Shutdown() })
	if err := rn.WaitForLeader(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	n := &Node{raft: rn, store: store, log: slog.Default()}
	n.cfg.NodeID = "auto-node"
	c := newAutomaticUpgradeController(n)
	rn.SetAutomaticUpgradeReady(c.ready.Load)
	c.ready.Store(true)
	now := time.Now()
	c.tick(ctx, now)
	// Missing audit blocks a mutation even after the stability interval.
	c.tick(ctx, now.Add(automaticUpgradeStable))
	state, _ := store.ContractState()
	if state.Prepared != nil || state.Active != 1 {
		t.Fatal("unaudited mutation", state)
	}
	sink, err := audit.NewLocalSink(audit.LocalConfig{Path: filepath.Join(t.TempDir(), "audit.jsonl")})
	if err != nil {
		t.Fatal(err)
	}
	n.auditLog, err = audit.NewAuditLog(ctx, audit.Config{Sinks: []audit.AuditSink{sink}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.auditLog.Close() })
	for i := 1; i <= 4; i++ {
		c.tick(ctx, now.Add(time.Duration(i)*automaticUpgradeStable))
	}
	state, _ = store.ContractState()
	if state.Active != 2 || state.Prepared != nil || state.Epoch != 1 {
		t.Fatal(state)
	}
	entries, err := n.auditLog.Query(ctx, "local", types.AuditFilter{Action: "cluster.upgrade.write"})
	if err != nil || len(entries) != 4 {
		t.Fatalf("%+v %v", entries, err)
	}
	for _, e := range entries {
		if e.ActorScope != "cluster:auto-node" || e.PolicyRule != "automatic-contract-1-to-2" {
			t.Fatal(e)
		}
	}
	c.tick(ctx, now.Add(10*automaticUpgradeStable))
	again, _ := n.auditLog.Query(ctx, "local", types.AuditFilter{Action: "cluster.upgrade.write"})
	if len(again) != len(entries) {
		t.Fatal("replayed completed upgrade")
	}
}

func TestAutomaticControllerRecoveryModeAndStop(t *testing.T) {
	n := &Node{cfg: Config{RestoreMode: true}, log: slog.Default()}
	c := newAutomaticUpgradeController(n)
	c.start(context.Background())
	if c.ready.Load() || c.done != nil {
		t.Fatal("restore started controller")
	}
	c.stop()
	n.cfg.RestoreMode = false
	c.start(context.Background())
	if c.ready.Load() || c.done != nil {
		t.Fatal("stopped controller restarted")
	}
}

// Exercise production startup and the real fixed clock intervals; shorter unit
// tests above isolate audit and recovery failures without changing a global timer.
func TestNodeAutomaticallyActivatesTeamsWithoutRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("real automatic-upgrade stabilization intervals")
	}
	cfg := auditTaskConfig(t)
	cfg.Functions = append(cfg.Functions, types.FunctionCompute, types.FunctionComputeTeams)
	cfg.LLMProvider = compute.NewMockProvider(compute.MockResponse{Content: "ready"})
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
		egress.SetActiveProvider(nil)
	})
	groups, inbox := n.groupSvc, n.inboxSvc
	if groups == nil || inbox == nil {
		t.Fatal("dormant dependencies not wired")
	}
	if _, ok := n.toolRegistry.Get("ask_bot"); ok {
		t.Fatal("teams exposed before activation")
	}
	deadline := time.NewTimer(100 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for !n.teamContractActive() {
		select {
		case <-deadline.C:
			t.Fatalf("automatic startup did not activate: %v", n.automaticUpgrade.blocker.Load())
		case <-poll.C:
		}
	}
	if groups != n.groupSvc || inbox != n.inboxSvc {
		t.Fatal("activation replaced live dependencies")
	}
	if _, ok := n.toolRegistry.Get("ask_bot"); !ok {
		t.Fatal("team tools unavailable after automatic activation")
	}
	if _, err := n.botSvc.Put(ctx, &pb.BotRecord{Id: "after-auto", Owner: "user:alice", Enabled: true}, 0); err != nil {
		t.Fatal(err)
	}
}
