package gateway

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func taskChatReply(task *pb.TaskApprovalRecord, turnID string) *pb.ConsoleBotReply {
	text := task.Result
	if task.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_COMPLETED {
		state := strings.ToLower(strings.TrimPrefix(task.State.String(), "TASK_APPROVAL_STATE_"))
		text = fmt.Sprintf("Task `%s`: %s. [Review task approvals](/approvals/%s).", task.Id, strings.ReplaceAll(state, "_", " "), url.PathEscape(task.Id))
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
	return &pb.ConsoleBotReply{Text: text, TurnId: turnID, ToolsUsed: used, ToolsAttempted: attempted, ToolCalls: int32(executed), CostUsd: task.GetBudgetSpent().GetSpendUsd(), SessionId: task.SessionId, Transcript: task.Transcript, Receipts: task.Receipts}
}
