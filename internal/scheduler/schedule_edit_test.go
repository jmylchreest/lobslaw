package scheduler

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestCompletionPreservesConcurrentScheduleEdit(t *testing.T) {
	node, store := singleNodeRaft(t, "schedule-edit")
	s, err := NewScheduler(Config{NodeID: "schedule-edit"}, node, NewHandlerRegistry())
	if err != nil {
		t.Fatal(err)
	}
	fired := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	seedTask(t, node, &pb.ScheduledTaskRecord{Id: "job", Schedule: "*/5 * * * *", Enabled: true, ClaimedBy: "schedule-edit", ClaimExpiresAt: timestamppb.New(time.Now().Add(time.Hour))})
	inFlight := loadTask(t, store, "job")
	edited := proto.Clone(inFlight).(*pb.ScheduledTaskRecord)
	edited.Schedule = "0 9 * * *"
	desired := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	edited.NextRun = timestamppb.New(desired)
	rev := inFlight.Revision
	if err := s.applyClaim(&pb.LogEntry{Op: pb.LogOp_LOG_OP_CLAIM, Id: "job", ExpectedRevision: &rev, ExpectedClaimer: "schedule-edit", Payload: &pb.LogEntry_ScheduledTask{ScheduledTask: edited}}); err != nil {
		t.Fatal(err)
	}
	s.completeTask(t.Context(), inFlight, fired)
	got := loadTask(t, store, "job")
	if !got.NextRun.AsTime().Equal(desired) || got.Schedule != edited.Schedule {
		t.Fatalf("completion overwrote cadence: %v", got)
	}
}
