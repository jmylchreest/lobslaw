package node

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/soul"
	"github.com/jmylchreest/lobslaw/internal/tools"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

const soulRPCTimeout time.Duration = 5 * time.Second

// The service runs on raft nodes. Reads and history selection go to the
// leader, so compute-only clients do not roll their cache back on failover.
type soulTuneServer struct {
	lobslawv1.UnimplementedSoulTuneServiceServer
	n *Node
}

func (s *soulTuneServer) leader(ctx context.Context) (lobslawv1.SoulTuneServiceClient, func(), error) {
	if s.n.raft.IsLeader() {
		return nil, func() {}, nil
	}
	addr := string(s.n.raft.LeaderAddress())
	if addr == "" {
		return nil, nil, status.Error(codes.Unavailable, "soul tune: no raft leader")
	}
	conn, err := s.n.dialer()(ctx, addr)
	if err != nil {
		return nil, nil, err
	}
	return lobslawv1.NewSoulTuneServiceClient(conn), func() { _ = conn.Close() }, nil
}

func (s *soulTuneServer) GetSoulTune(ctx context.Context, req *lobslawv1.GetSoulTuneRequest) (*lobslawv1.GetSoulTuneResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, soulRPCTimeout)
	defer cancel()
	client, closeConn, err := s.leader(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()
	if client != nil {
		return client.GetSoulTune(ctx, req)
	}
	if err := s.n.raft.Raft.VerifyLeader().Error(); err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	rec, err := s.n.soulTuneSvc.Get(ctx)
	if err != nil {
		return nil, soulRPCError(err)
	}
	return &lobslawv1.GetSoulTuneResponse{Record: rec}, nil
}

func (s *soulTuneServer) PutSoulTune(ctx context.Context, req *lobslawv1.PutSoulTuneRequest) (*lobslawv1.PutSoulTuneResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, soulRPCTimeout)
	defer cancel()
	if req.GetState() == nil {
		return nil, status.Error(codes.InvalidArgument, "state required")
	}
	client, closeConn, err := s.leader(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()
	if client != nil {
		return client.PutSoulTune(ctx, req)
	}
	rec, err := s.n.soulTuneSvc.Put(ctx, req.State, req.ExpectedRevision)
	if err != nil {
		return nil, soulRPCError(err)
	}
	return &lobslawv1.PutSoulTuneResponse{Record: rec}, nil
}

func (s *soulTuneServer) RollbackSoulTune(ctx context.Context, req *lobslawv1.RollbackSoulTuneRequest) (*lobslawv1.RollbackSoulTuneResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, soulRPCTimeout)
	defer cancel()
	if req.GetSteps() < 1 || req.GetSteps() > memory.MaxSoulTuneHistory {
		return nil, status.Error(codes.InvalidArgument, "steps outside retained history range")
	}
	client, closeConn, err := s.leader(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()
	if client != nil {
		return client.RollbackSoulTune(ctx, req)
	}
	rec, err := s.n.soulTuneSvc.Rollback(ctx, int(req.Steps))
	if err != nil {
		return nil, soulRPCError(err)
	}
	return &lobslawv1.RollbackSoulTuneResponse{Record: rec}, nil
}

func soulRPCError(err error) error {
	if errors.Is(err, memory.ErrClaimConflict) {
		return status.Error(codes.Aborted, "soul changed concurrently; read current state and retry")
	}
	return status.Error(codes.FailedPrecondition, err.Error())
}

// A compute-only node reads the cluster at turn/tool boundaries. There is no
// writable in-memory fallback: a successful edit must mean a durable edit.
type remoteSoulTuneStore struct {
	n    *Node
	seen atomic.Bool
}

func (s *remoteSoulTuneStore) candidates() []string {
	var out []string
	if s.n.registry != nil {
		for _, p := range s.n.registry.List() {
			if string(p.ID) != s.n.cfg.NodeID && (slices.Contains(p.Functions, types.FunctionMemory) || slices.Contains(p.Functions, types.FunctionPolicy)) {
				out = append(out, p.Address)
			}
		}
	}
	for _, addr := range s.n.cfg.SeedNodes {
		if !slices.Contains(out, addr) {
			out = append(out, addr)
		}
	}
	return out
}

func (s *remoteSoulTuneStore) call(ctx context.Context, retryRead bool, run func(context.Context, lobslawv1.SoulTuneServiceClient) error) error {
	ctx, cancel := context.WithTimeout(ctx, soulRPCTimeout)
	defer cancel()
	last := errors.New("soul tune requires a reachable memory/policy node; configure discovery seed_nodes")
	for _, addr := range s.candidates() {
		var conn *grpc.ClientConn
		conn, last = s.n.dialer()(ctx, addr)
		if last != nil {
			continue
		}
		last = run(ctx, lobslawv1.NewSoulTuneServiceClient(conn))
		if last == nil {
			s.seen.Store(true)
		}
		_ = conn.Close()
		// Never replay an ambiguous mutation on another peer. CAS protects
		// puts, but a repeated rollback could undo one change too many.
		if last == nil || (!retryRead && status.Code(last) != codes.Unimplemented) {
			return last
		}
	}
	return last
}

func tuneFromRecord(rec *lobslawv1.SoulTuneRecord) *soul.TuneState {
	if rec == nil {
		return nil
	}
	out := tuneStateFromProto(rec.Current)
	if out == nil {
		out = &soul.TuneState{}
	}
	out.Revision = rec.Revision
	return out
}

func (s *remoteSoulTuneStore) Get(ctx context.Context) (*soul.TuneState, error) {
	// A standalone node can use its file baseline. Mutations still fail.
	if len(s.candidates()) == 0 && !s.seen.Load() {
		return nil, nil
	}
	var state *soul.TuneState
	err := s.call(ctx, true, func(ctx context.Context, c lobslawv1.SoulTuneServiceClient) error {
		r, err := c.GetSoulTune(ctx, &lobslawv1.GetSoulTuneRequest{})
		if err == nil {
			state = tuneFromRecord(r.Record)
		}
		return err
	})
	return state, err
}

func (s *remoteSoulTuneStore) Put(ctx context.Context, state *soul.TuneState) error {
	if state == nil {
		return errors.New("soul tune: state required")
	}
	return s.call(ctx, false, func(ctx context.Context, c lobslawv1.SoulTuneServiceClient) error {
		r, err := c.PutSoulTune(ctx, &lobslawv1.PutSoulTuneRequest{State: tuneStateToProto(state), ExpectedRevision: state.Revision})
		if err == nil {
			*state = *tuneFromRecord(r.Record)
		}
		return err
	})
}

func (s *remoteSoulTuneStore) Rollback(ctx context.Context, steps int) (*soul.TuneState, error) {
	if steps < 1 || steps > memory.MaxSoulTuneHistory {
		return nil, fmt.Errorf("soul tune: invalid rollback steps %d", steps)
	}
	var state *soul.TuneState
	err := s.call(ctx, false, func(ctx context.Context, c lobslawv1.SoulTuneServiceClient) error {
		r, err := c.RollbackSoulTune(ctx, &lobslawv1.RollbackSoulTuneRequest{Steps: uint32(steps)})
		if err == nil {
			state = tuneFromRecord(r.Record)
		}
		return err
	})
	return state, err
}

// Only the soul tool family opts into remote policy evaluation. This keeps
// unrelated compute-only tools closed until their own execution paths support it.
func (n *Node) remoteSoulPolicy(ctx context.Context, claims *types.Claims, action, resource string) (policy.Decision, error) {
	supported := false
	for _, def := range tools.SoulToolDefs() {
		if resource == def.Name {
			supported = true
			break
		}
	}
	if action != "tool:exec" || !supported {
		return policy.Decision{}, compute.ErrNoPolicyEngine
	}
	if claims == nil {
		return policy.Decision{}, errors.New("soul: caller identity required for policy evaluation")
	}
	ctx, cancel := context.WithTimeout(ctx, soulRPCTimeout)
	defer cancel()
	last := errors.New("soul: no policy node available")
	for _, addr := range (&remoteSoulTuneStore{n: n}).candidates() {
		conn, err := n.dialer()(ctx, addr)
		if err != nil {
			last = err
			continue
		}
		dec, err := lobslawv1.NewPolicyServiceClient(conn).Evaluate(ctx, &lobslawv1.EvaluateRequest{
			Claims: &lobslawv1.Claims{UserId: claims.UserID, Scope: claims.Scope, Roles: claims.Roles},
			Action: action, Resource: resource,
		})
		_ = conn.Close()
		if err != nil {
			last = err
			continue
		}
		return policy.Decision{Effect: types.Effect(dec.Effect), RuleID: dec.RuleId, Reason: dec.Reason}, nil
	}
	return policy.Decision{}, last
}
