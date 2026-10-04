package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type taskCreateInbox struct {
	InboxService
	items []*pb.BotInboxItem
}

func (s *taskCreateInbox) Post(_ context.Context, item *pb.BotInboxItem) (*pb.BotInboxItem, error) {
	item.Id = "queued-task"
	item.Status = pb.InboxStatus_INBOX_STATUS_PENDING
	s.items = append(s.items, item)
	return item, nil
}

func TestTaskCreateUsesTurnAuthorityAndRefusesRecursiveTasks(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, owner, body string
		inTask, wantOK    bool
	}{
		{name: "own task", owner: "user:alice", body: "Prepare a backup plan", wantOK: true},
		{name: "other owner", owner: "user:bob", body: "Prepare a backup plan"},
		{name: "missing work", owner: "user:alice", body: "  "},
		{name: "already a task", owner: "user:alice", body: "Prepare a backup plan", inTask: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc := &taskCreateInbox{}
			profiles := messagingProfiles{"worker": {ID: "worker", Owner: test.owner}}
			claims := &types.Claims{UserID: "alice", Roles: []string{"operator"}}
			ctx := turn.WithIdentity(t.Context(), turn.Identity{BotID: "worker", Principal: identity.Bot("worker"), BotOwner: identity.User("alice"), OriginalClaims: claims})
			if test.inTask {
				ctx = compute.WithTaskExecution(ctx, turn.TaskScope{ID: "existing-task"}, nil)
			}
			raw, code, err := newTaskCreateHandler(svc, profiles)(ctx, map[string]string{
				"body": test.body, "subject": "Backup plan", "bot_id": "other", "owner": "user:bob",
			})
			if !test.wantOK {
				if err == nil || code == 0 || len(svc.items) != 0 {
					t.Fatalf("invalid task accepted: %s, %d, %v", raw, code, err)
				}
				return
			}
			if err != nil || code != 0 || len(svc.items) != 1 {
				t.Fatalf("task not queued: %s, %d, %v", raw, code, err)
			}
			item := svc.items[0]
			if item.Recipient != "worker" || item.Sender != "bot:worker" || item.RequestedBy != "user:alice" || item.TaskClaims.GetUserId() != "alice" || item.Kind != pb.InboxKind_INBOX_KIND_TASK || item.Body != test.body {
				t.Fatalf("task lost work or authority: %v", item)
			}
			var out map[string]any
			if err := json.Unmarshal(raw, &out); err != nil || out["id"] != item.Id || out["status"] != "pending" {
				t.Fatalf("missing queue receipt: %s, %v", raw, err)
			}
		})
	}
}
