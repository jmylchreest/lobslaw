package memory

import (
	"context"
	"errors"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/ids"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// LinkTask fences the ordinary queue before any external work starts. A crash
// after this write is reconciled from the task record, never a fresh attempt.
func (s *InboxService) LinkTask(ctx context.Context, item *pb.BotInboxItem, taskID string) (*pb.BotInboxItem, error) {
	if s.raft == nil || item == nil || taskID == "" {
		return nil, errors.New("inbox: task link requires raft, item and task ID")
	}
	next := proto.Clone(item).(*pb.BotInboxItem)
	next.TaskId = taskID
	next.Status = pb.InboxStatus_INBOX_STATUS_WAITING
	next.ClaimExpiresAt = nil
	if item.Id == "" {
		if err := validateInboxItem(next); err != nil {
			return nil, err
		}
		count, err := s.countPending(next.Recipient)
		if err != nil {
			return nil, err
		}
		if count >= s.maxPending {
			return nil, ErrInboxFull
		}
		next.Id, next.CreatedAt, next.Revision = ids.New(), timestamppb.Now(), 1
		if err := s.apply(ctx, pb.LogOp_LOG_OP_PUT, next, nil, ""); err != nil {
			return nil, err
		}
	} else {
		if item.Status != pb.InboxStatus_INBOX_STATUS_CLAIMED || item.TaskId != "" {
			return nil, ErrClaimConflict
		}
		next.Revision++
		expected := item.Revision
		if err := s.apply(ctx, pb.LogOp_LOG_OP_CLAIM, next, &expected, item.ClaimedBy); err != nil {
			return nil, err
		}
	}
	return next, nil
}
