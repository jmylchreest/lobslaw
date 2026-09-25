package memory

import (
	"math"
	"testing"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestTaskBudgetRefusesUnboundedOrOverflowingExtension(t *testing.T) {
	r := &pb.TaskApprovalRecord{BudgetLimits: &pb.TaskBudget{ToolCalls: 3, SpendUsd: 1, EgressBytes: 10}, Continuation: &pb.Continuation{ToolCalls: 4, SpentUsd: 2, EgressBytes: 20}}
	for _, extra := range []*pb.TaskBudget{nil, {}, {ToolCalls: -1}, {SpendUsd: math.NaN()}, {SpendUsd: math.Inf(1)}, {ToolCalls: math.MaxInt32}, {EgressBytes: math.MaxInt64}, {ToolCalls: 1}} {
		if _, e := extendTaskBudget(r, extra); e == nil {
			t.Fatalf("invalid extension accepted: %v", extra)
		}
	}
	n, e := extendTaskBudget(r, &pb.TaskBudget{ToolCalls: 2, SpendUsd: 0.5, EgressBytes: 30})
	if e != nil {
		t.Fatal(e)
	}
	if n.ToolCalls != 6 || n.SpendUsd != 2.5 || n.EgressBytes != 50 {
		t.Fatalf("extra allowance not added to consumed work: %v", n)
	}
}
