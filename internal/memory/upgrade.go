package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/dataformat"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

const upgradeTimeout = 10 * time.Second

type UpgradePeer struct {
	ID        string
	Supported []uint32
	State     dataformat.ContractState
}

func (p *UpgradePeer) supports(target uint32) error {
	if p == nil || !slices.Contains(p.Supported, target) {
		return fmt.Errorf("member does not support contract %d", target)
	}
	return nil
}

// SetUpgradeProbe is installed before the gRPC server starts accepting calls.
func (n *RaftNode) SetUpgradeProbe(probe func(context.Context, raft.Server) (*UpgradePeer, error)) {
	n.upgradeProbe = probe
}

func (n *RaftNode) UpgradeStatus() (*pb.UpgradeStatusResponse, error) {
	state, err := n.fsm.store.ContractState()
	if err != nil {
		return nil, err
	}
	out := &pb.UpgradeStatusResponse{NodeId: string(n.nodeID), SupportedContracts: dataformat.SupportedContracts(), ActiveContract: state.Active, Epoch: state.Epoch, AppliedIndex: n.fsm.lastApplied(), LeaderAddress: string(n.LeaderAddress())}
	if t := state.Prepared; t != nil {
		out.Prepared = transitionCommand("prepare", *t)
		out.PreparedIndex = t.Index
	}
	return out, nil
}

func transitionCommand(action string, t dataformat.Transition) *pb.UpgradeCommand {
	return &pb.UpgradeCommand{Action: action, TransitionId: t.ID, ExpectedEpoch: t.Epoch, Target: t.Target, MembershipIndex: t.MembershipIndex, MemberIds: slices.Clone(t.Members)}
}

func (n *RaftNode) validateProposal(raw []byte) error {
	var entry pb.LogEntry
	if err := proto.Unmarshal(raw, &entry); err != nil {
		return err
	}
	if entry.GetUpgrade() != nil {
		return errors.New("upgrade control requires the operator upgrade service")
	}
	state, err := n.fsm.store.ContractState()
	if err != nil {
		return err
	}
	return validateContractEntry(&entry, state)
}

func (n *RaftNode) ChangeUpgrade(ctx context.Context, req *pb.ChangeUpgradeRequest) (*pb.UpgradeStatusResponse, error) {
	if req == nil {
		return nil, errors.New("upgrade request required")
	}
	n.controlMu.Lock()
	defer n.controlMu.Unlock()
	if err := n.Raft.Barrier(upgradeTimeout).Error(); err != nil {
		return nil, err
	}
	state, err := n.fsm.store.ContractState()
	if err != nil {
		return nil, err
	}
	if state.CompletedID == req.TransitionId && state.CompletedAction == req.Action && state.Epoch == req.ExpectedEpoch+1 {
		return n.UpgradeStatus()
	}
	cfg := n.Raft.GetConfiguration()
	if err := cfg.Error(); err != nil {
		return nil, err
	}
	servers := cfg.Configuration().Servers
	ids := make([]string, 0, len(servers))
	for _, s := range servers {
		ids = append(ids, string(s.ID))
	}
	slices.Sort(ids)
	t := dataformat.Transition{ID: req.TransitionId, Target: req.Target, Epoch: req.ExpectedEpoch, MembershipIndex: cfg.Index(), Members: ids, Index: 1}
	if _, err := state.Advance(req.Action, t, dataformat.SupportedContracts()); err != nil {
		return nil, err
	}
	// Abort is possible without unavailable peers, but still requires quorum and
	// the exact prepared membership. It does not advance the data contract.
	if req.Action != "abort" {
		if err := n.checkUpgradeMembers(ctx, servers, state, t, req.Action == "finalize"); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := proto.Marshal(&pb.LogEntry{Op: pb.LogOp_LOG_OP_PUT, Payload: &pb.LogEntry_Upgrade{Upgrade: transitionCommand(req.Action, t)}})
	if err != nil {
		return nil, err
	}
	result, err := n.applyRaw(raw, upgradeTimeout)
	if err != nil {
		return nil, err
	}
	if applyErr, ok := result.(error); ok {
		return nil, applyErr
	}
	return n.UpgradeStatus()
}

func (n *RaftNode) checkUpgradeMembers(ctx context.Context, servers []raft.Server, state dataformat.ContractState, t dataformat.Transition, finalize bool) error {
	for _, server := range servers {
		var peer *UpgradePeer
		if server.ID == n.nodeID {
			peer = &UpgradePeer{ID: string(n.nodeID), Supported: dataformat.SupportedContracts(), State: state}
		} else {
			if n.upgradeProbe == nil {
				return errors.New("upgrade member probe unavailable")
			}
			var err error
			probeCtx, cancel := context.WithTimeout(ctx, upgradeTimeout)
			peer, err = n.upgradeProbe(probeCtx, server)
			cancel()
			if err != nil {
				return fmt.Errorf("member %s is not ready: %w", server.ID, err)
			}
		}
		if peer == nil || peer.ID != string(server.ID) {
			return fmt.Errorf("member %s identity mismatch", server.ID)
		}
		if err := peer.supports(t.Target); err != nil {
			return fmt.Errorf("member %s: %w", server.ID, err)
		}
		if peer.State.Active != state.Active || peer.State.Epoch != state.Epoch {
			return fmt.Errorf("member %s has not caught up to the active contract", server.ID)
		}
		if finalize {
			p := peer.State.Prepared
			if p == nil || p.ID != t.ID || p.Target != t.Target || p.MembershipIndex != t.MembershipIndex || !slices.Equal(p.Members, t.Members) || p.Index != state.Prepared.Index {
				return fmt.Errorf("member %s has not durably prepared this transition", server.ID)
			}
		}
	}
	return nil
}
