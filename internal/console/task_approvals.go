package console

import (
	"context"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type TaskApprovalAPI interface {
	GetTaskApproval(context.Context, *pb.GetTaskApprovalRequest) (*pb.GetTaskApprovalResponse, error)
	ListTaskApproval(context.Context, *pb.ListTaskApprovalRequest) (*pb.ListTaskApprovalResponse, error)
	DecideTaskApproval(context.Context, *pb.DecideTaskApprovalRequest) (*pb.DecideTaskApprovalResponse, error)
	CancelTaskApproval(context.Context, *pb.CancelTaskApprovalRequest) (*pb.CancelTaskApprovalResponse, error)
	RecoverTaskApproval(context.Context, *pb.RecoverTaskApprovalRequest) (*pb.RecoverTaskApprovalResponse, error)
}
