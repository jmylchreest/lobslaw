package compute

import pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"

func taskBudgetProto(c BudgetCaps) *pb.TaskBudget {
	return &pb.TaskBudget{ToolCalls: int32(c.MaxToolCalls), SpendUsd: c.MaxSpendUSD, EgressBytes: c.MaxEgressBytes}
}

// Approved limits apply to this task, but a newly tightened operator policy
// triggers another bounded approval instead of silently retaining older limits.
func effectiveTaskBudget(current BudgetCaps, r *pb.TaskApprovalRecord) BudgetCaps {
	old, limits := r.BudgetPolicy, r.BudgetLimits
	if limits == nil {
		return current
	}
	out := BudgetCaps{MaxToolCalls: int(limits.ToolCalls), MaxSpendUSD: limits.SpendUsd, MaxEgressBytes: limits.EgressBytes}
	if current.MaxToolCalls > 0 && (old.GetToolCalls() == 0 || current.MaxToolCalls < int(old.GetToolCalls())) {
		out.MaxToolCalls = current.MaxToolCalls
	}
	if current.MaxSpendUSD > 0 && (old.GetSpendUsd() == 0 || current.MaxSpendUSD < old.GetSpendUsd()) {
		out.MaxSpendUSD = current.MaxSpendUSD
	}
	if current.MaxEgressBytes > 0 && (old.GetEgressBytes() == 0 || current.MaxEgressBytes < old.GetEgressBytes()) {
		out.MaxEgressBytes = current.MaxEgressBytes
	}
	return out
}
