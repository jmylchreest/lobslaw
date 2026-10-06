package node

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// PolicyService must answer from the engine the node evaluates with.
// It used to build a private Engine over the same store, which shared
// the stored rules but never received SetDefaults or registered
// conditions, so `lobslaw policy rules` on a live node listed stored
// rules only while the in-memory fallbacks still applied.
func TestPolicyServiceUsesTheEvaluatingEngine(t *testing.T) {
	ctx := context.Background()
	store := crossOwnerTestStore(t)
	_, transport := raft.NewInmemTransport("policy-node")
	rn, err := memory.NewRaft(memory.RaftConfig{NodeID: "policy-node", LocalAddr: "policy-node", DataDir: t.TempDir(), Bootstrap: true, Transport: transport}, memory.NewFSM(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rn.Shutdown() })
	if err := rn.WaitForLeader(5 * time.Second); err != nil {
		t.Fatal(err)
	}

	n := &Node{raft: rn, store: store, log: slog.Default(), server: grpc.NewServer()}
	n.cfg.Functions = []types.NodeFunction{types.FunctionMemory}
	if err := n.wirePolicyService(); err != nil {
		t.Fatal(err)
	}
	if n.policyEngine == nil || n.policySvc.Engine() != n.policyEngine {
		t.Fatal("policy service and node evaluate with different engines")
	}
	if n.ensurePolicyEngine() != n.policyEngine {
		t.Fatal("compute wiring would replace the shared engine")
	}
	if err := n.wirePolicyDefaults(); err != nil {
		t.Fatal(err)
	}

	stored := &lobslawv1.PolicyRule{Id: "operator-low", Subject: "user:alice", Action: "tool:exec", Resource: "*", Effect: "deny", Priority: 1}
	if _, err := n.policySvc.AddRule(ctx, &lobslawv1.AddRuleRequest{Rule: stored}); err != nil {
		t.Fatal(err)
	}

	res, err := n.policySvc.SyncRules(ctx, &lobslawv1.SyncRulesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(res.GetRules()))
	for _, r := range res.GetRules() {
		ids = append(ids, r.GetId())
	}
	if len(ids) < 2 || ids[0] != "operator-low" {
		t.Fatalf("stored rules come first, in evaluation order; got %v", ids)
	}
	foundDefault := false
	for _, id := range ids[1:] {
		if id == "default-operator-cluster-upgrade-read" {
			foundDefault = true
		}
	}
	if !foundDefault {
		t.Fatalf("SyncRules omitted the in-memory defaults the node enforces: %v", ids)
	}

	// Evaluate over gRPC sees the same fallbacks the node applies.
	dec, err := n.policySvc.Evaluate(ctx, &lobslawv1.EvaluateRequest{
		Claims: &lobslawv1.Claims{UserId: "op", Roles: []string{"operator"}}, Action: "cluster.upgrade.read", Resource: "cluster:*",
	})
	if err != nil || dec.GetEffect() != string(types.EffectAllow) || dec.GetRuleId() != "default-operator-cluster-upgrade-read" {
		t.Fatalf("Evaluate did not use the defaults: %+v, %v", dec, err)
	}
}
