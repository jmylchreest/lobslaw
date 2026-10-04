package gateway

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Only the owner-facing subset of TaskApprovalService is exposed. Execution
// claims, grants and checkpoints remain between the trusted runner and service.
func (s *Server) queryConsoleTask(ctx context.Context, in *pb.QueryConsoleRequest) (*pb.QueryConsoleResponse, error) {
	if s.cfg.TaskApprovals == nil {
		return nil, status.Error(codes.Unavailable, "task approvals unavailable")
	}
	switch q := in.Query.(type) {
	case *pb.QueryConsoleRequest_TaskApprovals:
		request := proto.Clone(q.TaskApprovals).(*pb.ListTaskApprovalRequest)
		request.Owner = in.Identity.Principal
		response, err := s.cfg.TaskApprovals.ListTaskApproval(ctx, request)
		return &pb.QueryConsoleResponse{Result: &pb.QueryConsoleResponse_TaskApprovals{TaskApprovals: response}}, err
	case *pb.QueryConsoleRequest_TaskApproval:
		request := proto.Clone(q.TaskApproval).(*pb.GetTaskApprovalRequest)
		request.Owner = in.Identity.Principal
		response, err := s.cfg.TaskApprovals.GetTaskApproval(ctx, request)
		return &pb.QueryConsoleResponse{Result: &pb.QueryConsoleResponse_TaskApproval{TaskApproval: response}}, err
	default:
		return nil, status.Error(codes.InvalidArgument, "task query required")
	}
}

func (s *Server) mutateConsoleTask(ctx context.Context, in *pb.MutateConsoleRequest) (*pb.MutateConsoleResponse, error) {
	if s.cfg.TaskApprovals == nil {
		return nil, status.Error(codes.Unavailable, "task approvals unavailable")
	}
	switch q := in.Operation.(type) {
	case *pb.MutateConsoleRequest_DecideTaskApproval:
		request := proto.Clone(q.DecideTaskApproval).(*pb.DecideTaskApprovalRequest)
		request.Owner = in.Identity.Principal
		response, err := s.cfg.TaskApprovals.DecideTaskApproval(ctx, request)
		return &pb.MutateConsoleResponse{Result: &pb.MutateConsoleResponse_TaskDecision{TaskDecision: response}}, err
	case *pb.MutateConsoleRequest_CancelTaskApproval:
		request := proto.Clone(q.CancelTaskApproval).(*pb.CancelTaskApprovalRequest)
		request.Owner = in.Identity.Principal
		response, err := s.cfg.TaskApprovals.CancelTaskApproval(ctx, request)
		return &pb.MutateConsoleResponse{Result: &pb.MutateConsoleResponse_TaskCancel{TaskCancel: response}}, err
	case *pb.MutateConsoleRequest_RecoverTaskApproval:
		request := proto.Clone(q.RecoverTaskApproval).(*pb.RecoverTaskApprovalRequest)
		request.Owner = in.Identity.Principal
		response, err := s.cfg.TaskApprovals.RecoverTaskApproval(ctx, request)
		return &pb.MutateConsoleResponse{Result: &pb.MutateConsoleResponse_TaskRecovery{TaskRecovery: response}}, err
	default:
		return nil, status.Error(codes.InvalidArgument, "task decision required")
	}
}

type remoteConsoleTasks struct {
	client   pb.ConsoleServiceClient
	identity *pb.ConsoleIdentity
}

func (c *remoteConsoleTasks) ListTaskApproval(ctx context.Context, q *pb.ListTaskApprovalRequest) (*pb.ListTaskApprovalResponse, error) {
	out, err := c.client.QueryConsole(ctx, &pb.QueryConsoleRequest{Identity: c.identity, Query: &pb.QueryConsoleRequest_TaskApprovals{TaskApprovals: q}})
	if err == nil && out.GetTaskApprovals() == nil {
		err = status.Error(codes.Internal, "missing task list")
	}
	return out.GetTaskApprovals(), err
}
func (c *remoteConsoleTasks) GetTaskApproval(ctx context.Context, q *pb.GetTaskApprovalRequest) (*pb.GetTaskApprovalResponse, error) {
	out, err := c.client.QueryConsole(ctx, &pb.QueryConsoleRequest{Identity: c.identity, Query: &pb.QueryConsoleRequest_TaskApproval{TaskApproval: q}})
	if err == nil && out.GetTaskApproval() == nil {
		err = status.Error(codes.Internal, "missing task")
	}
	return out.GetTaskApproval(), err
}
func (c *remoteConsoleTasks) DecideTaskApproval(ctx context.Context, q *pb.DecideTaskApprovalRequest) (*pb.DecideTaskApprovalResponse, error) {
	out, err := c.client.MutateConsole(ctx, &pb.MutateConsoleRequest{Identity: c.identity, Operation: &pb.MutateConsoleRequest_DecideTaskApproval{DecideTaskApproval: q}})
	if err == nil && out.GetTaskDecision() == nil {
		err = status.Error(codes.Internal, "missing task decision")
	}
	return out.GetTaskDecision(), err
}
func (c *remoteConsoleTasks) CancelTaskApproval(ctx context.Context, q *pb.CancelTaskApprovalRequest) (*pb.CancelTaskApprovalResponse, error) {
	out, err := c.client.MutateConsole(ctx, &pb.MutateConsoleRequest{Identity: c.identity, Operation: &pb.MutateConsoleRequest_CancelTaskApproval{CancelTaskApproval: q}})
	if err == nil && out.GetTaskCancel() == nil {
		err = status.Error(codes.Internal, "missing task cancellation")
	}
	return out.GetTaskCancel(), err
}
func (c *remoteConsoleTasks) RecoverTaskApproval(ctx context.Context, q *pb.RecoverTaskApprovalRequest) (*pb.RecoverTaskApprovalResponse, error) {
	out, err := c.client.MutateConsole(ctx, &pb.MutateConsoleRequest{Identity: c.identity, Operation: &pb.MutateConsoleRequest_RecoverTaskApproval{RecoverTaskApproval: q}})
	if err == nil && out.GetTaskRecovery() == nil {
		err = status.Error(codes.Internal, "missing task recovery")
	}
	return out.GetTaskRecovery(), err
}
