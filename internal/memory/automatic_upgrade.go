package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jmylchreest/lobslaw/internal/dataformat"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// SetAutomaticUpgradeReady is boot-time wiring. The accessor must be safe for
// concurrent calls and stay false during startup, recovery and shutdown.
func (n *RaftNode) SetAutomaticUpgradeReady(ready func() bool) { n.automaticReady = ready }

// AutomaticUpgradePlan is a read-only observation. Applying it rechecks the
// durable state, every member and the leadership/membership fence.
type AutomaticUpgradePlan struct {
	Step         dataformat.AutomaticStep
	Fence        string
	AppliedIndex uint64
}

func (v controlView) automaticFence() string {
	raw, _ := json.Marshal(v.state)
	return fmt.Sprintf("%d/%s/%s", v.generation, configurationFingerprint(v.servers), raw)
}

func (n *RaftNode) PlanAutomaticUpgrade(ctx context.Context) (*AutomaticUpgradePlan, error) {
	ctx, cancel := controlContext(ctx, upgradeTimeout)
	defer cancel()
	lease, before, err := n.readControl(ctx)
	if err != nil {
		return nil, err
	}
	lease.release()
	step, reason := before.state.AutomaticStep()
	if reason != "" {
		return nil, errors.New(reason)
	}
	if err := n.checkAutomaticMembers(ctx, before); err != nil {
		return nil, err
	}
	// Do not begin the stability interval until every member has prepared.
	if step.Action == "finalize" {
		if err := n.checkUpgradeMembers(ctx, before.servers, before.state, *before.state.Prepared, true); err != nil {
			return nil, err
		}
	}
	lease, after, err := n.readControl(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.release()
	if !before.matches(after) {
		return nil, errControlChanged
	}
	return &AutomaticUpgradePlan{Step: step, Fence: before.automaticFence(), AppliedIndex: before.applied}, nil
}

// ChangeAutomaticUpgrade is internal cluster control, never registered as an
// external RPC or tool. The node adapter must audit admission before calling it.
func (n *RaftNode) ChangeAutomaticUpgrade(ctx context.Context, plan *AutomaticUpgradePlan) (*pb.UpgradeStatusResponse, error) {
	if plan == nil || plan.Fence == "" {
		return nil, errors.New("automatic plan required")
	}
	s := plan.Step
	return n.changeUpgrade(ctx, &pb.ChangeUpgradeRequest{Action: s.Action, TransitionId: s.ID, Target: s.Target, ExpectedEpoch: s.Epoch}, plan.Fence, plan.AppliedIndex)
}

func (n *RaftNode) checkAutomaticMembers(ctx context.Context, view controlView) error {
	step, reason := view.state.AutomaticStep()
	if reason != "" {
		return errors.New(reason)
	}
	for _, member := range view.servers {
		local := member.ID == n.nodeID
		peer := &UpgradePeer{ID: string(n.nodeID), Supported: dataformat.SupportedContracts(), State: view.state, AutomaticTargets: dataformat.AutomaticTargets(), AutomaticReady: n.automaticReady != nil && n.automaticReady(), AppliedIndex: view.applied}
		if !local {
			if n.upgradeProbe == nil {
				return errors.New("automatic member probe unavailable")
			}
			var err error
			peer, err = n.upgradeProbe(ctx, member)
			if err != nil {
				return fmt.Errorf("member %s unavailable: %w", member.ID, err)
			}
		}
		if peer == nil || peer.ID != string(member.ID) {
			return fmt.Errorf("member %s identity mismatch", member.ID)
		}
		if !peer.AutomaticReady || !slices.Contains(peer.AutomaticTargets, step.Target) {
			return fmt.Errorf("member %s is not ready for automatic contract %d", member.ID, step.Target)
		}
		if err := peer.supports(step.Target); err != nil {
			return err
		}
		if peer.State.Active != view.state.Active || peer.State.Epoch != view.state.Epoch || peer.AppliedIndex < view.applied {
			return fmt.Errorf("member %s has not caught up", member.ID)
		}
	}
	return nil
}
