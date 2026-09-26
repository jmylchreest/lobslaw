package gateway

import (
	"fmt"
	"slices"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func taskChatReply(task *pb.TaskApprovalRecord, turnID string) map[string]any {
	text := task.Result
	if task.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_COMPLETED {
		state := strings.ToLower(strings.TrimPrefix(task.State.String(), "TASK_APPROVAL_STATE_"))
		text = fmt.Sprintf("Task `%s`: %s. [Review task approvals](/approvals).", task.Id, strings.ReplaceAll(state, "_", " "))
	}
	used, attempted := []string{}, []string{}
	executed := 0
	for _, receipt := range task.Receipts {
		if receipt.ExecutionStatus == turn.ReceiptExecuted {
			executed++
			if !slices.Contains(used, receipt.ToolName) {
				used = append(used, receipt.ToolName)
			}
		} else if !slices.Contains(attempted, receipt.ToolName) {
			attempted = append(attempted, receipt.ToolName)
		}
	}
	return map[string]any{"text": text, "turn_id": turnID, "tools_used": used, "tools_attempted": attempted, "tool_calls": executed, "cost_usd": task.GetBudgetSpent().GetSpendUsd(), "session_id": task.SessionId, "transcript": task.Transcript, "receipts": task.Receipts}
}
