package memory

import (
	"testing"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestPortableInboxNeverCarriesExecutionAuthority(t *testing.T) {
	t.Parallel()
	item := &pb.BotInboxItem{TaskId: "task", Status: pb.InboxStatus_INBOX_STATUS_WAITING, TaskClaims: &pb.Claims{UserId: "alice", Roles: []string{"operator"}}}
	if err := portableMessage(item.ProtoReflect()); err != nil {
		t.Fatal(err)
	}
	if item.TaskClaims != nil {
		t.Fatal("portable export retained caller authority")
	}
	// Import is equally strict even when the input was not produced by us.
	item.TaskClaims = &pb.Claims{UserId: "alice"}
	paused, err := pauseArchiveRecord("inbox", item, "")
	if err != nil || !paused || item.Status != pb.InboxStatus_INBOX_STATUS_CANCELLED || item.TaskClaims != nil {
		t.Fatalf("import retained runnable authority: %v %v", item, err)
	}
}
