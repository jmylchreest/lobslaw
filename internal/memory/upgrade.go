package memory

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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
	ID               string
	Supported        []uint32
	State            dataformat.ContractState
	AutomaticTargets []uint32
	AutomaticReady   bool
	AppliedIndex     uint64
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
	out.AutomaticTargets = dataformat.AutomaticTargets()
	out.AutomaticReady = n.automaticReady != nil && n.automaticReady()
	_, out.AutomaticBlocker = state.AutomaticStep()
	if !out.AutomaticReady {
		out.AutomaticBlocker = "local node is not ready for automatic activation"
	}
	if t := state.Prepared; t != nil {
		out.Prepared = transitionCommand("prepare", *t)
		out.PreparedIndex = t.Index
	}
	return out, nil
}

func transitionCommand(action string, t dataformat.Transition) *pb.UpgradeCommand {
	return &pb.UpgradeCommand{Action: action, TransitionId: t.ID, ExpectedEpoch: t.Epoch, Target: t.Target, MembershipIndex: t.MembershipIndex, MembershipFingerprint: t.MembershipFingerprint, MemberIds: slices.Clone(t.Members)}
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
	if req != nil && req.Action == "prepare" && dataformat.IsAutomaticID(req.TransitionId) {
		return nil, errors.New("automatic transition IDs are reserved for cluster control")
	}
	return n.changeUpgrade(ctx, req, "", 0)
}

func (n *RaftNode) changeUpgrade(ctx context.Context, req *pb.ChangeUpgradeRequest, automaticFence string, automaticIndex uint64) (*pb.UpgradeStatusResponse, error) {
	if req == nil {
		return nil, errors.New("upgrade request required")
	}
	ctx, cancel := controlContext(ctx, upgradeTimeout)
	defer cancel()
	lease, before, err := n.readControl(ctx)
	if err != nil {
		return nil, err
	}
	lease.release()
	if req.Action == "transfer" {
		return n.transferUpgradeLeader(ctx, before, req.TargetNodeId)
	}
	if automaticFence != "" {
		if before.automaticFence() != automaticFence {
			return nil, errControlChanged
		}
		step, reason := before.state.AutomaticStep()
		if reason != "" || req.Action != step.Action || req.TransitionId != step.ID || req.Target != step.Target || req.ExpectedEpoch != step.Epoch {
			return nil, errors.New("automatic request does not match durable transition")
		}
	}
	state := before.state
	ids := make([]string, 0, len(before.servers))
	for _, server := range before.servers {
		ids = append(ids, string(server.ID))
	}
	slices.Sort(ids)
	t := dataformat.Transition{ID: req.TransitionId, Target: req.Target, Epoch: req.ExpectedEpoch, MembershipIndex: before.index, MembershipFingerprint: configurationFingerprint(before.servers), Members: ids, Index: 1}
	if state.Prepared != nil {
		t.MembershipIndex = state.Prepared.MembershipIndex
	} else if state.CompletedID == req.TransitionId && state.Completed != nil {
		t.MembershipIndex = state.Completed.MembershipIndex
	}
	if _, err := state.Advance(req.Action, t, dataformat.SupportedContracts()); err != nil {
		return nil, err
	}
	if state.CompletedID == req.TransitionId {
		return n.UpgradeStatus()
	}
	// Abort needs quorum and the prepared membership, but not unavailable peers.
	if req.Action != "abort" {
		if err := n.checkUpgradeMembers(ctx, before.servers, state, t, req.Action == "finalize"); err != nil {
			return nil, err
		}
	}
	if automaticFence != "" {
		// Audit admission and ordinary writes may advance the log after planning.
		// Require the observed plan watermark, not a moving post-audit target.
		readiness := before
		if automaticIndex > before.applied {
			return nil, errControlChanged
		}
		readiness.applied = automaticIndex
		if err := n.checkAutomaticMembers(ctx, readiness); err != nil {
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
	raw, err := proto.Marshal(&pb.LogEntry{Op: pb.LogOp_LOG_OP_PUT, Payload: &pb.LogEntry_Upgrade{Upgrade: transitionCommand(req.Action, t)}})
	if err != nil {
		return nil, err
	}
	result, err := n.applyControl(ctx, lease, before.generation, raw)
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
			if p == nil || p.ID != t.ID || p.Target != t.Target || p.MembershipIndex != t.MembershipIndex || p.MembershipFingerprint != t.MembershipFingerprint || !slices.Equal(p.Members, t.Members) || p.Index != state.Prepared.Index {
				return fmt.Errorf("member %s has not durably prepared this transition", server.ID)
			}
		}
	}
	return nil
}

// The pinned Raft GetConfiguration API returns index zero. Fence preparation
// with the applied barrier index and compare the complete configuration,
// including addresses and suffrage, on every continuation.
func configurationFingerprint(servers []raft.Server) string {
	servers = slices.Clone(servers)
	slices.SortFunc(servers, func(a, b raft.Server) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	raw, _ := json.Marshal(servers)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func (n *RaftNode) transferUpgradeLeader(ctx context.Context, before controlView, id string) (*pb.UpgradeStatusResponse, error) {
	if id == "" {
		return nil, errors.New("target node id required")
	}
	if id == string(n.nodeID) {
		return n.UpgradeStatus()
	}
	for _, member := range before.servers {
		if string(member.ID) != id || member.Suffrage != raft.Voter {
			continue
		}
		if n.upgradeProbe == nil {
			return nil, errors.New("member verification unavailable")
		}
		remote, err := n.upgradeProbe(ctx, member)
		if err != nil {
			return nil, err
		}
		if remote == nil || remote.ID != id {
			return nil, errors.New("member identity mismatch")
		}
		if err := remote.supports(before.state.Required()); err != nil {
			return nil, err
		}
		lease, after, err := n.readControl(ctx)
		if err != nil {
			return nil, err
		}
		defer lease.release()
		if !before.matches(after) {
			return nil, errControlChanged
		}
		future, err := n.enqueueControl(ctx, before.generation, func(time.Duration) raft.Future { return n.Raft.LeadershipTransferToServer(member.ID, member.Address) })
		if err != nil {
			return nil, err
		}
		if err := lease.wait(ctx, future); err != nil {
			return nil, err
		}
		return n.UpgradeStatus()
	}
	return nil, errors.New("target is not a configured voter")
}
