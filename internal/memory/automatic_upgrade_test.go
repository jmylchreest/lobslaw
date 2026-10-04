package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/jmylchreest/lobslaw/internal/dataformat"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestAutomaticUpgradeRequiresReadinessAndPreservesAbort(t *testing.T) {
	node := newSingleNodeRaft(t)
	if err := node.WaitForLeader(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := node.PlanAutomaticUpgrade(ctx); err == nil {
		t.Fatal("unready node accepted")
	}
	node.SetAutomaticUpgradeReady(func() bool { return true })
	plan, err := node.PlanAutomaticUpgrade(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.ChangeUpgrade(ctx, &pb.ChangeUpgradeRequest{Action: "prepare", TransitionId: plan.Step.ID, Target: 2}); err == nil {
		t.Fatal("external automatic preparation accepted")
	}
	if _, err := node.ChangeAutomaticUpgrade(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := node.ChangeAutomaticUpgrade(ctx, plan); err == nil {
		t.Fatal("stale plan replayed")
	}
	final, err := node.PlanAutomaticUpgrade(ctx)
	if err != nil || final.Step.Action != "finalize" {
		t.Fatalf("%+v %v", final, err)
	}
	if _, err := node.ChangeUpgrade(ctx, &pb.ChangeUpgradeRequest{Action: "abort", TransitionId: final.Step.ID, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := node.PlanAutomaticUpgrade(ctx); err == nil {
		t.Fatal("automatic retry undid abort")
	}
	if _, err := node.ChangeAutomaticUpgrade(ctx, final); err == nil {
		t.Fatal("stale finalization undid abort")
	}
}

func TestAutomaticUpgradeChecksEveryMemberAndLeadership(t *testing.T) {
	nodes := controlCluster(t)
	leader := nodes[0]
	for _, n := range nodes {
		n.SetAutomaticUpgradeReady(func() bool { return true })
	}
	if err := leader.Raft.AddNonvoter(nodes[3].nodeID, nodes[3].localAddr, 0, time.Second).Error(); err != nil {
		t.Fatal(err)
	}
	mode := "old"
	leader.SetUpgradeProbe(func(ctx context.Context, member raft.Server) (*UpgradePeer, error) {
		if member.ID == nodes[3].nodeID && mode == "offline" {
			return nil, errors.New("offline")
		}
		state, err := leader.fsm.store.ContractState()
		if err != nil {
			return nil, err
		}
		peer := &UpgradePeer{ID: string(member.ID), State: state, Supported: dataformat.SupportedContracts(), AutomaticTargets: dataformat.AutomaticTargets(), AutomaticReady: true, AppliedIndex: leader.fsm.lastApplied()}
		if member.ID == nodes[3].nodeID {
			switch mode {
			case "old":
				peer.AutomaticTargets = nil
			case "unready":
				peer.AutomaticReady = false
			case "lagging":
				peer.State.Epoch++
			case "identity":
				peer.ID = "imposter"
			}
		}
		return peer, nil
	})
	for _, m := range []string{"old", "offline", "unready", "lagging", "identity"} {
		mode = m
		if _, err := leader.PlanAutomaticUpgrade(context.Background()); err == nil {
			t.Fatalf("%s nonvoter accepted", m)
		}
	}
	mode = "ready"
	plan, err := leader.PlanAutomaticUpgrade(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	transferControlLeader(t, leader, nodes[1])
	if _, err := leader.ChangeAutomaticUpgrade(context.Background(), plan); err == nil {
		t.Fatal("former leader applied plan")
	}
}
