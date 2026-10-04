package node

import (
	"context"
	"slices"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/dataformat"
	"github.com/jmylchreest/lobslaw/internal/grpcinterceptors"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type upgradeService struct{ node *Node }

func (s *upgradeService) authorize(ctx context.Context, write bool) error {
	cert := grpcinterceptors.VerifiedPeerCert(ctx)
	if cert == nil {
		return status.Error(codes.PermissionDenied, "upgrade control requires a verified certificate")
	}
	if !write && !mtls.IsOperatorCert(cert) {
		return nil
	}
	n := s.node
	if !mtls.IsOperatorCert(cert) || n.policyEngine == nil {
		return status.Error(codes.PermissionDenied, "upgrade control requires an operator certificate and policy grant")
	}
	roles := n.resolveUserRoles(cert.Subject.CommonName)
	if !slices.Contains(roles, "operator") {
		return status.Error(codes.PermissionDenied, "operator role required")
	}
	action := "cluster.upgrade.read"
	if write {
		action = "cluster.upgrade.write"
	}
	decision, err := n.policyEngine.Evaluate(ctx, &types.Claims{UserID: cert.Subject.CommonName, Roles: roles}, action, "cluster:*")
	if err != nil || decision.Effect != types.EffectAllow {
		return status.Error(codes.PermissionDenied, "explicit cluster upgrade policy grant required")
	}
	return nil
}

func (s *upgradeService) UpgradeStatus(ctx context.Context, _ *pb.UpgradeStatusRequest) (*pb.UpgradeStatusResponse, error) {
	if err := s.authorize(ctx, false); err != nil {
		return nil, err
	}
	return s.node.raft.UpgradeStatus()
}
func (s *upgradeService) ChangeUpgrade(ctx context.Context, req *pb.ChangeUpgradeRequest) (*pb.ChangeUpgradeResponse, error) {
	if err := s.authorize(ctx, true); err != nil {
		return nil, err
	}
	out, err := s.node.raft.ChangeUpgrade(ctx, req)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "upgrade blocked: %v", err)
	}
	return &pb.ChangeUpgradeResponse{Status: out}, nil
}

func (n *Node) wireUpgradeService() {
	if n.raft == nil {
		return
	}
	n.raft.SetUpgradeProbe(n.probeUpgradeMember)
	pb.RegisterUpgradeServiceServer(n.server, &upgradeService{node: n})
}

func (n *Node) probeUpgradeMember(ctx context.Context, server raft.Server) (*memory.UpgradePeer, error) {
	conn, err := n.dialer()(ctx, string(server.Address))
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	var remote peer.Peer
	reply, err := pb.NewUpgradeServiceClient(conn).UpgradeStatus(ctx, &pb.UpgradeStatusRequest{}, grpc.Peer(&remote))
	if err != nil {
		return nil, err
	}
	tls, ok := remote.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tls.State.VerifiedChains) == 0 || len(tls.State.VerifiedChains[0]) == 0 || tls.State.VerifiedChains[0][0].Subject.CommonName != string(server.ID) || reply.NodeId != string(server.ID) {
		return nil, status.Error(codes.PermissionDenied, "upgrade peer identity does not match Raft member")
	}
	state := dataformat.ContractState{Active: reply.ActiveContract, Epoch: reply.Epoch}
	if p := reply.Prepared; p != nil {
		state.Prepared = &dataformat.Transition{ID: p.TransitionId, Target: p.Target, Epoch: p.ExpectedEpoch, MembershipIndex: p.MembershipIndex, MembershipFingerprint: p.MembershipFingerprint, Members: p.MemberIds, Index: reply.PreparedIndex}
	}
	if err := state.Validate(reply.SupportedContracts); err != nil {
		return nil, err
	}
	return &memory.UpgradePeer{ID: reply.NodeId, Supported: reply.SupportedContracts, State: state}, nil
}

func (n *Node) dataContract() uint32 {
	if n.store == nil {
		return 1
	}
	state, err := n.store.ContractState()
	if err != nil {
		return 0
	} // zero fails closed
	return state.Required()
}
