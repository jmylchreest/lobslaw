package memory

import (
	"errors"
	"testing"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestWaitingTaskOccupiesCapacityAtRaftBoundary(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 1)
	_, err := svc.LinkTask(t.Context(), &pb.BotInboxItem{Recipient: "worker", Body: "waiting for approval"}, "task")
	if err != nil {
		t.Fatal(err)
	}
	err = svc.apply(t.Context(), pb.LogOp_LOG_OP_PUT, &pb.BotInboxItem{Id: "late", Recipient: "worker", Body: "late task", Status: pb.InboxStatus_INBOX_STATUS_WAITING, TaskId: "second"}, nil, "")
	if !errors.Is(err, ErrInboxFull) {
		t.Fatalf("waiting task bypassed capacity: %v", err)
	}
}
