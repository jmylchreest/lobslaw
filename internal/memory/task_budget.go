package memory

import (
	"math"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func validTaskBudget(b *pb.TaskBudget) bool {
	return b.GetToolCalls() >= 0 && b.GetSpendUsd() >= 0 && !math.IsNaN(b.GetSpendUsd()) && !math.IsInf(b.GetSpendUsd(), 0) && b.GetEgressBytes() >= 0
}

// An extension starts from consumed work or the previous limit, whichever is
// greater. Zero is never interpreted as removing a limit in an approval.
func extendTaskBudget(r *pb.TaskApprovalRecord, extra *pb.TaskBudget) (*pb.TaskBudget, error) {
	if !validTaskBudget(extra) || (extra.GetToolCalls() == 0 && extra.GetSpendUsd() == 0 && extra.GetEgressBytes() == 0) {
		return nil, status.Error(codes.InvalidArgument, "a positive finite extra allowance is required")
	}
	old := r.BudgetLimits
	spent := r.Continuation
	if spent == nil || spent.ToolCalls < 0 || spent.SpentUsd < 0 || math.IsNaN(spent.SpentUsd) || math.IsInf(spent.SpentUsd, 0) || spent.EgressBytes < 0 {
		return nil, status.Error(codes.FailedPrecondition, "invalid consumed budget")
	}
	n := &pb.TaskBudget{ToolCalls: old.GetToolCalls(), SpendUsd: old.GetSpendUsd(), EgressBytes: old.GetEgressBytes()}
	if extra.ToolCalls > 0 {
		base := max(int64(n.ToolCalls), int64(spent.ToolCalls))
		sum := base + int64(extra.ToolCalls)
		if sum > math.MaxInt32 {
			return nil, status.Error(codes.InvalidArgument, "tool allowance overflow")
		}
		n.ToolCalls = int32(sum)
	}
	if extra.SpendUsd > 0 {
		n.SpendUsd = max(n.SpendUsd, spent.SpentUsd) + extra.SpendUsd
		if math.IsInf(n.SpendUsd, 0) {
			return nil, status.Error(codes.InvalidArgument, "spend allowance overflow")
		}
	}
	if extra.EgressBytes > 0 {
		base := max(n.EgressBytes, spent.EgressBytes)
		if extra.EgressBytes > math.MaxInt64-base {
			return nil, status.Error(codes.InvalidArgument, "egress allowance overflow")
		}
		n.EgressBytes = base + extra.EgressBytes
	}
	if (n.ToolCalls > 0 && n.ToolCalls <= spent.ToolCalls) || (n.SpendUsd > 0 && n.SpendUsd <= spent.SpentUsd) || (n.EgressBytes > 0 && n.EgressBytes <= spent.EgressBytes) {
		return nil, status.Error(codes.InvalidArgument, "extend each exhausted budget dimension before resuming")
	}
	return n, nil
}
