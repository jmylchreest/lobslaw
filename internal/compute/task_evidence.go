package compute

import (
	"context"
	"time"

	"github.com/jmylchreest/lobslaw/internal/promptguard"
	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

const TaskCommitTimeout = 5 * time.Second

func invocationResult(inv *ToolInvocation, result *InvokeResult) {
	if result == nil {
		return
	}
	inv.ExitCode, inv.Output = result.ExitCode, combineOutputs(result)
	if result.started {
		inv.ExecutionStatus = turn.ReceiptExecuted
	}
}

func taskTranscriptStart(task *pb.TaskApprovalRecord, response *ProcessMessageResponse) uint32 {
	if len(task.Transcript) > 0 {
		return task.TranscriptStart
	}
	return uint32(response.TurnStartIndex)
}

func taskReceipts(calls []ToolInvocation) []*pb.TurnToolInvocation {
	var out []*pb.TurnToolInvocation
	for _, call := range calls {
		out = append(out, &pb.TurnToolInvocation{CallId: call.CallID, ToolName: call.ToolName, Args: promptguard.Redact(call.Args), Output: promptguard.Redact(call.Output), ExitCode: int32(call.ExitCode), Error: promptguard.Redact(call.Error), ExecutionStatus: call.ExecutionStatus})
	}
	return out
}

// FinishTask commits the observed result, not just the model's final sentence.
// A failed leg is fenced as uncertain and retains an existing recovery checkpoint.
func FinishTask(ctx context.Context, backend TaskApprovalBackend, task *pb.TaskApprovalRecord, response *ProcessMessageResponse, runErr error) (*pb.TaskApprovalRecord, error) {
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), TaskCommitTimeout)
	defer cancel()
	q := &pb.FinishTaskApprovalRequest{Id: task.Id, Owner: task.Owner, Actor: task.Actor, Revision: task.Revision, ClaimToken: task.ClaimedBy, Result: response.Reply, Receipts: taskReceipts(response.ToolCalls), TranscriptStart: taskTranscriptStart(task, response), BudgetSpent: taskBudgetProto(BudgetCaps{MaxToolCalls: response.BudgetState.ToolCalls, MaxSpendUSD: response.BudgetState.SpendUSD, MaxEgressBytes: response.BudgetState.EgressBytes})}
	q.TurnId = task.TurnId
	if len(response.Messages) == 0 {
		q.BudgetSpent = task.BudgetSpent
	}
	for _, message := range response.Messages {
		q.Transcript = append(q.Transcript, turn.MessageToProto(message))
	}
	if runErr != nil {
		q.OutcomeUnknown = true
		q.Result = "Execution outcome uncertain: " + promptguard.Redact(runErr.Error())
	}
	out, err := backend.FinishTaskApproval(commitCtx, q)
	if err != nil {
		return nil, err
	}
	return out.Record, nil
}
