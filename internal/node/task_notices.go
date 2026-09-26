package node

import (
	"context"
	"fmt"

	"github.com/jmylchreest/lobslaw/internal/gateway"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type taskNoticeSource struct{ n *Node }

func (s taskNoticeSource) Notices(ctx context.Context, principal string) ([]gateway.Notice, error) {
	if principal == "" {
		return nil, nil
	}
	after := ""
	for {
		page, err := s.n.taskApprovalAPI().ListTaskApproval(ctx, &pb.ListTaskApprovalRequest{Owner: principal, AfterId: after})
		if err != nil {
			return nil, err
		}
		for _, task := range page.Records {
			switch task.State {
			case pb.TaskApprovalState_TASK_APPROVAL_STATE_WAITING, pb.TaskApprovalState_TASK_APPROVAL_STATE_OUTCOME_UNKNOWN:
				return []gateway.Notice{{Text: fmt.Sprintf("Task %s (%s) needs your attention: %s. Review it in the task approvals view or /v1/task-approvals/%s.", task.Id, task.Actor, task.State, task.Id)}}, nil
			}
		}
		if page.NextAfterId == "" {
			return nil, nil
		}
		after = page.NextAfterId
	}
}
