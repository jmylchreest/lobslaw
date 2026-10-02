package node

import (
	"context"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/memory"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// All operations use the leader, including grant reads: a stale follower must
// not authorise execution after cancellation. Never retry an ambiguous write.
type taskApprovalServer struct {
	n     *Node
	store *memory.TaskApprovalStore
}

func (n *Node) wireTaskApprovals() error {
	store, err := memory.NewTaskApprovalStore(n.raft, n.store)
	if err != nil {
		return err
	}
	n.taskApprovals = &taskApprovalServer{n: n, store: store}
	pb.RegisterTaskApprovalServiceServer(n.server, n.taskApprovals)
	return nil
}
func (s *taskApprovalServer) backend(ctx context.Context) (pb.TaskApprovalServiceClient, func(), error) {
	if s.n.raft != nil && s.n.raft.IsLeader() {
		if err := s.n.raft.Raft.VerifyLeader().Error(); err != nil {
			return nil, nil, status.Error(codes.Unavailable, "task approval leader unavailable")
		}
		return nil, func() {}, nil
	}
	var addresses []string
	if s.n.raft != nil {
		if addr := string(s.n.raft.LeaderAddress()); addr != "" {
			addresses = append(addresses, addr)
		}
	}
	if s.n.registry != nil {
		for _, p := range s.n.registry.List() {
			if string(p.ID) != s.n.cfg.NodeID && (slices.Contains(p.Functions, types.FunctionMemory) || slices.Contains(p.Functions, types.FunctionPolicy)) {
				addresses = append(addresses, p.Address)
			}
		}
	}
	addresses = append(addresses, s.n.cfg.SeedNodes...)
	for _, addr := range addresses {
		conn, err := s.n.dialer()(ctx, addr)
		if err == nil {
			return pb.NewTaskApprovalServiceClient(conn), func() { _ = conn.Close() }, nil
		}
	}
	return nil, nil, status.Error(codes.Unavailable, "no reachable task approval leader")
}

func (s *taskApprovalServer) CreateTaskApproval(ctx context.Context, q *pb.CreateTaskApprovalRequest) (*pb.CreateTaskApprovalResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, closeConn, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()
	if client != nil {
		if len(metadata.ValueFromIncomingContext(ctx, "task-approval-forwarded")) > 0 {
			return nil, status.Error(codes.Unavailable, "task approval leader changed; refresh")
		}
		ctx = metadata.AppendToOutgoingContext(ctx, "task-approval-forwarded", "1")
		return client.CreateTaskApproval(ctx, q)
	}
	return s.store.CreateTaskApproval(ctx, q)
}

func (s *taskApprovalServer) PauseTaskApproval(ctx context.Context, q *pb.PauseTaskApprovalRequest) (*pb.PauseTaskApprovalResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, closeConn, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()
	if client != nil {
		if len(metadata.ValueFromIncomingContext(ctx, "task-approval-forwarded")) > 0 {
			return nil, status.Error(codes.Unavailable, "task approval leader changed; refresh")
		}
		ctx = metadata.AppendToOutgoingContext(ctx, "task-approval-forwarded", "1")
		return client.PauseTaskApproval(ctx, q)
	}
	return s.store.PauseTaskApproval(ctx, q)
}

func (s *taskApprovalServer) GetTaskApproval(ctx context.Context, q *pb.GetTaskApprovalRequest) (*pb.GetTaskApprovalResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, closeConn, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()
	if client != nil {
		if len(metadata.ValueFromIncomingContext(ctx, "task-approval-forwarded")) > 0 {
			return nil, status.Error(codes.Unavailable, "task approval leader changed; refresh")
		}
		ctx = metadata.AppendToOutgoingContext(ctx, "task-approval-forwarded", "1")
		return client.GetTaskApproval(ctx, q)
	}
	return s.store.GetTaskApproval(ctx, q)
}

func (s *taskApprovalServer) ListTaskApproval(ctx context.Context, q *pb.ListTaskApprovalRequest) (*pb.ListTaskApprovalResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, closeConn, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()
	if client != nil {
		if len(metadata.ValueFromIncomingContext(ctx, "task-approval-forwarded")) > 0 {
			return nil, status.Error(codes.Unavailable, "task approval leader changed; refresh")
		}
		ctx = metadata.AppendToOutgoingContext(ctx, "task-approval-forwarded", "1")
		return client.ListTaskApproval(ctx, q)
	}
	return s.store.ListTaskApproval(ctx, q)
}

func (s *taskApprovalServer) DecideTaskApproval(ctx context.Context, q *pb.DecideTaskApprovalRequest) (*pb.DecideTaskApprovalResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, closeConn, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()
	if client != nil {
		if len(metadata.ValueFromIncomingContext(ctx, "task-approval-forwarded")) > 0 {
			return nil, status.Error(codes.Unavailable, "task approval leader changed; refresh")
		}
		ctx = metadata.AppendToOutgoingContext(ctx, "task-approval-forwarded", "1")
		return client.DecideTaskApproval(ctx, q)
	}
	return s.store.DecideTaskApproval(ctx, q)
}

func (s *taskApprovalServer) ClaimTaskApproval(ctx context.Context, q *pb.ClaimTaskApprovalRequest) (*pb.ClaimTaskApprovalResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, closeConn, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()
	if client != nil {
		if len(metadata.ValueFromIncomingContext(ctx, "task-approval-forwarded")) > 0 {
			return nil, status.Error(codes.Unavailable, "task approval leader changed; refresh")
		}
		ctx = metadata.AppendToOutgoingContext(ctx, "task-approval-forwarded", "1")
		return client.ClaimTaskApproval(ctx, q)
	}
	return s.store.ClaimTaskApproval(ctx, q)
}

func (s *taskApprovalServer) FinishTaskApproval(ctx context.Context, q *pb.FinishTaskApprovalRequest) (*pb.FinishTaskApprovalResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, closeConn, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()
	if client != nil {
		if len(metadata.ValueFromIncomingContext(ctx, "task-approval-forwarded")) > 0 {
			return nil, status.Error(codes.Unavailable, "task approval leader changed; refresh")
		}
		ctx = metadata.AppendToOutgoingContext(ctx, "task-approval-forwarded", "1")
		return client.FinishTaskApproval(ctx, q)
	}
	return s.store.FinishTaskApproval(ctx, q)
}

func (s *taskApprovalServer) CancelTaskApproval(ctx context.Context, q *pb.CancelTaskApprovalRequest) (*pb.CancelTaskApprovalResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, closeConn, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()
	if client != nil {
		if len(metadata.ValueFromIncomingContext(ctx, "task-approval-forwarded")) > 0 {
			return nil, status.Error(codes.Unavailable, "task approval leader changed; refresh")
		}
		ctx = metadata.AppendToOutgoingContext(ctx, "task-approval-forwarded", "1")
		return client.CancelTaskApproval(ctx, q)
	}
	return s.store.CancelTaskApproval(ctx, q)
}

func (s *taskApprovalServer) RecoverTaskApproval(ctx context.Context, q *pb.RecoverTaskApprovalRequest) (*pb.RecoverTaskApprovalResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, closeConn, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()
	if client != nil {
		if len(metadata.ValueFromIncomingContext(ctx, "task-approval-forwarded")) > 0 {
			return nil, status.Error(codes.Unavailable, "task approval leader changed; refresh")
		}
		ctx = metadata.AppendToOutgoingContext(ctx, "task-approval-forwarded", "1")
		return client.RecoverTaskApproval(ctx, q)
	}
	return s.store.RecoverTaskApproval(ctx, q)
}

func (s *taskApprovalServer) CheckGrantTaskApproval(ctx context.Context, q *pb.CheckGrantTaskApprovalRequest) (*pb.CheckGrantTaskApprovalResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, closeConn, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()
	if client != nil {
		if len(metadata.ValueFromIncomingContext(ctx, "task-approval-forwarded")) > 0 {
			return nil, status.Error(codes.Unavailable, "task approval leader changed; refresh")
		}
		ctx = metadata.AppendToOutgoingContext(ctx, "task-approval-forwarded", "1")
		return client.CheckGrantTaskApproval(ctx, q)
	}
	return s.store.CheckGrantTaskApproval(ctx, q)
}

func (n *Node) taskApprovalAPI() *taskApprovalServer {
	if n.taskApprovals != nil {
		return n.taskApprovals
	}
	return &taskApprovalServer{n: n}
}
