package compute

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// TurnServer serves AgentService on a compute node, using the local
// runner. The peer certificate authenticates the calling NODE. The
// user is the Claims/Principal on the request — never the peer.
type TurnServer struct {
	runner turn.Runner
}

// NewTurnServer wraps a local runner as AgentService. Nil runner is
// refused at each RPC rather than at construction so a mis-wired
// node still answers Ping.
func NewTurnServer(runner turn.Runner) *TurnServer {
	return &TurnServer{runner: runner}
}

func (s *TurnServer) Ping(context.Context, *lobslawv1.PingRequest) (*lobslawv1.PingResponse, error) {
	if s == nil || s.runner == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node has no local compute")
	}
	return &lobslawv1.PingResponse{}, nil
}

func (s *TurnServer) RunTurn(ctx context.Context, req *lobslawv1.RunTurnRequest) (*lobslawv1.RunTurnResponse, error) {
	runner, err := s.ready()
	if err != nil {
		return nil, err
	}
	turnReq, err := authenticatedRequest(req)
	if err != nil {
		return nil, err
	}
	resp, err := runner.Run(ctx, turnReq)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "run turn: %v", err)
	}
	return responseToProto(resp), nil
}

func (s *TurnServer) ResumeTurn(ctx context.Context, req *lobslawv1.ResumeTurnRequest) (*lobslawv1.ResumeTurnResponse, error) {
	runner, err := s.ready()
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "resume request required")
	}
	turnReq, err := authenticatedRequest(req.Request)
	if err != nil {
		return nil, err
	}
	var prior []turn.Message
	for _, m := range req.Prior {
		prior = append(prior, messageFromProto(m))
	}
	ctx = turn.WithTurnApproval(ctx, req.ApprovalAction, req.ApprovalResource)
	resp, err := runner.Resume(ctx, turnReq, prior)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resume turn: %v", err)
	}
	return &lobslawv1.ResumeTurnResponse{Response: responseToProto(resp)}, nil
}

func (s *TurnServer) ready() (turn.Runner, error) {
	if s == nil || s.runner == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node has no local compute")
	}
	return s.runner, nil
}

// authenticatedRequest rebuilds a turn from the wire request and
// refuses an empty user. The peer certificate authenticates the
// calling node and is never substituted for Claims — that would grant
// the unrestricted audience to a machine identity.
func authenticatedRequest(req *lobslawv1.RunTurnRequest) (turn.Request, error) {
	if req == nil || req.Claims == nil || req.Claims.UserId == "" {
		return turn.Request{}, status.Error(codes.InvalidArgument,
			"claims.user_id is required — the peer node is not the user")
	}
	return requestFromProto(req), nil
}
