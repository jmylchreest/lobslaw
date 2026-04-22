package discovery

import (
	"context"
	"log/slog"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/logging"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// RaftMembership is the subset of RaftNode behaviour AddMember needs.
// Kept as an interface so tests can supply a fake without pulling the
// full Raft stack in.
type RaftMembership interface {
	IsLeader() bool
	LeaderAddress() raft.ServerAddress
	AddVoter(id raft.ServerID, addr raft.ServerAddress) error
}

// ReloadFunc is the hook Phase 11 hot-reload plugs into. Returns the
// lists that go into ReloadResponse. If the Service was constructed
// with nil, Reload returns codes.Unimplemented.
type ReloadFunc func(ctx context.Context, sections []string) (reloaded, restartNeeded []string, errs map[string]string)

// Service implements lobslawv1.NodeServiceServer. It holds the local
// peer registry and delegates Reload to a caller-supplied function.
type Service struct {
	lobslawv1.UnimplementedNodeServiceServer

	registry *Registry
	local    types.NodeInfo
	logger   *slog.Logger
	reload   ReloadFunc

	// raft is optional — non-nil when this node runs the memory or
	// policy function. AddMember returns Unimplemented when it's nil.
	raft RaftMembership
}

// NewService constructs the gRPC server-side implementation. reload
// may be nil until Phase 11 wires it. raftMembership may be nil on
// nodes without Raft (compute-only, gateway-only), in which case
// AddMember returns Unimplemented.
func NewService(registry *Registry, local types.NodeInfo, logger *slog.Logger, reload ReloadFunc, raftMembership RaftMembership) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		registry: registry,
		local:    local,
		logger:   logger,
		reload:   reload,
		raft:     raftMembership,
	}
}

// Register adds the caller's node info to the local registry.
func (s *Service) Register(ctx context.Context, req *lobslawv1.RegisterRequest) (*lobslawv1.RegisterResponse, error) {
	if req == nil || req.Node == nil {
		return &lobslawv1.RegisterResponse{Accepted: false, Reason: "missing node info"}, nil
	}
	peer := fromProto(req.Node)
	if peer.ID == "" {
		return &lobslawv1.RegisterResponse{Accepted: false, Reason: "empty node id"}, nil
	}
	s.registry.Register(peer)
	logging.From(ctx).Info("peer registered",
		"peer_id", peer.ID,
		"addr", peer.Address,
		"functions", peer.Functions,
	)
	return &lobslawv1.RegisterResponse{Accepted: true}, nil
}

// Deregister removes the caller from the registry.
func (s *Service) Deregister(ctx context.Context, req *lobslawv1.DeregisterRequest) (*lobslawv1.DeregisterResponse, error) {
	if req == nil || req.NodeId == "" {
		return nil, status.Error(codes.InvalidArgument, "node_id is required")
	}
	s.registry.Deregister(types.NodeID(req.NodeId))
	logging.From(ctx).Info("peer deregistered", "peer_id", req.NodeId)
	return &lobslawv1.DeregisterResponse{}, nil
}

// Heartbeat updates the caller's LastSeen. If the peer isn't in the
// registry (e.g. the receiving node restarted), return a soft failure
// so the caller can re-register.
func (s *Service) Heartbeat(_ context.Context, req *lobslawv1.HeartbeatRequest) (*lobslawv1.HeartbeatResponse, error) {
	if req == nil || req.NodeId == "" {
		return nil, status.Error(codes.InvalidArgument, "node_id is required")
	}
	if !s.registry.Heartbeat(types.NodeID(req.NodeId)) {
		return nil, status.Errorf(codes.NotFound, "peer %q not registered; re-register first", req.NodeId)
	}
	return &lobslawv1.HeartbeatResponse{}, nil
}

// GetPeers returns the caller's view of known peers. Each peer list
// includes this node itself so seed-dialers learn us on first contact.
func (s *Service) GetPeers(_ context.Context, _ *lobslawv1.GetPeersRequest) (*lobslawv1.GetPeersResponse, error) {
	peers := s.registry.List()
	out := make([]*lobslawv1.NodeInfo, 0, len(peers)+1)
	if s.local.ID != "" {
		out = append(out, toProto(s.local))
	}
	for _, p := range peers {
		out = append(out, toProto(p.NodeInfo))
	}
	return &lobslawv1.GetPeersResponse{Peers: out}, nil
}

// Reload dispatches to the registered ReloadFunc. Returns
// Unimplemented when no hook is wired (Phase 2.5–2.6 state).
func (s *Service) Reload(ctx context.Context, req *lobslawv1.ReloadRequest) (*lobslawv1.ReloadResponse, error) {
	if s.reload == nil {
		return nil, status.Error(codes.Unimplemented, "reload not wired (lands in Phase 11)")
	}
	var sections []string
	if req != nil {
		sections = req.Sections
	}
	reloaded, restartNeeded, errs := s.reload(ctx, sections)
	return &lobslawv1.ReloadResponse{
		Reloaded:      reloaded,
		RestartNeeded: restartNeeded,
		Errors:        errs,
	}, nil
}

// AddMember requests that the cluster add the given node as a voter.
// Must be executed on the current leader — followers reply
// FailedPrecondition with the leader's address so the caller retries.
// Non-voting learners are future work; for now Voter=false is rejected.
func (s *Service) AddMember(ctx context.Context, req *lobslawv1.AddMemberRequest) (*lobslawv1.AddMemberResponse, error) {
	if s.raft == nil {
		return nil, status.Error(codes.Unimplemented, "this node doesn't host the Raft group")
	}
	if req == nil || req.NodeId == "" || req.Address == "" {
		return nil, status.Error(codes.InvalidArgument, "node_id and address are required")
	}
	if !req.Voter {
		return nil, status.Error(codes.Unimplemented, "non-voting learners not yet supported")
	}

	if !s.raft.IsLeader() {
		return &lobslawv1.AddMemberResponse{
			Accepted:      false,
			LeaderAddress: string(s.raft.LeaderAddress()),
		}, nil
	}

	if err := s.raft.AddVoter(raft.ServerID(req.NodeId), raft.ServerAddress(req.Address)); err != nil {
		return nil, status.Errorf(codes.Internal, "AddVoter: %v", err)
	}
	logging.From(ctx).Info("cluster member added",
		"peer_id", req.NodeId,
		"address", req.Address,
	)
	return &lobslawv1.AddMemberResponse{Accepted: true}, nil
}
