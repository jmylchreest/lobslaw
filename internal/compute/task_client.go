package compute

import (
	"context"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// RemoteTaskApprovals adapts the typed peer client to TaskApprovalBackend. The
// supplied connection must authenticate as a cluster peer. No write is retried.
type RemoteTaskApprovals struct{ Client pb.TaskApprovalServiceClient }

func (c RemoteTaskApprovals) PauseTaskApproval(ctx context.Context, q *pb.PauseTaskApprovalRequest) (*pb.PauseTaskApprovalResponse, error) {
	return c.Client.PauseTaskApproval(ctx, q)
}

func (c RemoteTaskApprovals) ClaimTaskApproval(ctx context.Context, q *pb.ClaimTaskApprovalRequest) (*pb.ClaimTaskApprovalResponse, error) {
	return c.Client.ClaimTaskApproval(ctx, q)
}

func (c RemoteTaskApprovals) FinishTaskApproval(ctx context.Context, q *pb.FinishTaskApprovalRequest) (*pb.FinishTaskApprovalResponse, error) {
	return c.Client.FinishTaskApproval(ctx, q)
}

func (c RemoteTaskApprovals) CheckGrantTaskApproval(ctx context.Context, q *pb.CheckGrantTaskApprovalRequest) (*pb.CheckGrantTaskApprovalResponse, error) {
	return c.Client.CheckGrantTaskApproval(ctx, q)
}

func (c RemoteTaskApprovals) GetTaskApproval(ctx context.Context, q *pb.GetTaskApprovalRequest) (*pb.GetTaskApprovalResponse, error) {
	return c.Client.GetTaskApproval(ctx, q)
}
