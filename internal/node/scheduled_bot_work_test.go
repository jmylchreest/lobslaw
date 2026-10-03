package node

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/notify"
	"github.com/jmylchreest/lobslaw/internal/scheduler"
	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func scheduleRecord(t *testing.T, n *Node, id string) *pb.ScheduledTaskRecord {
	t.Helper()
	raw, err := n.store.Get(memory.BucketScheduledTasks, id)
	if err != nil {
		t.Fatal(err)
	}
	var task pb.ScheduledTaskRecord
	if err := proto.Unmarshal(raw, &task); err != nil {
		t.Fatal(err)
	}
	return &task
}

func putTestSchedule(t *testing.T, n *Node, task *pb.ScheduledTaskRecord) {
	t.Helper()
	raw, err := proto.Marshal(&pb.LogEntry{Op: pb.LogOp_LOG_OP_PUT, Id: task.Id, Payload: &pb.LogEntry_ScheduledTask{ScheduledTask: task}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.raft.Apply(raw, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestRecurringBotWorkSurvivesRestartAndDeliversOutcome(t *testing.T) {
	var scheduleID string
	provider := compute.NewMockProviderFunc(func(req compute.ChatRequest, index int) (compute.MockResponse, error) {
		if index == 0 {
			return compute.MockResponse{ToolCalls: []compute.ToolCall{{ID: "checkpoint", Name: "schedule_update", Arguments: `{"id":"` + scheduleID + `","checkpoint":"processed revision abc123"}`}}}, nil
		}
		return compute.MockResponse{Content: "Daily outcome: checked the assigned work."}, nil
	})
	cfg := auditTaskConfig(t)
	calls, forbidden := 0, 0
	wire := func(n *Node, _ *compute.AgentConfig) {
		if err := n.wireScheduleTools(n.builtinsRegistry); err != nil {
			t.Fatal(err)
		}
	}
	n, stop := bootTeamTaskNode(t, cfg, provider, &calls, &forbidden, wire)
	if _, err := n.botSvc.Put(t.Context(), &pb.BotRecord{Id: "worker", Owner: "user:alice", Enabled: true}, 0); err != nil {
		t.Fatal(err)
	}
	caller := turn.WithIdentity(t.Context(), turn.Identity{Principal: identity.Bot("worker"), BotID: "worker", BotOwner: identity.User("alice"), UserID: "alice", Timezone: "Europe/London"})
	create, _ := n.builtinsRegistry.Get("schedule_create")
	out, _, err := create(caller, map[string]string{"name": "Daily check", "when": "daily 09:00", "prompt": "Check assigned work and report the outcome.", "notify_on": "always"})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	scheduleID = result.ID
	task := scheduleRecord(t, n, scheduleID)
	if task.Owner != "bot:worker" || task.Params["requested_by"] != "user:alice" || !strings.HasPrefix(task.Schedule, "CRON_TZ=Europe/London ") {
		t.Fatalf("identity/timezone lost: %+v", task)
	}
	task.NextRun = timestamppb.New(time.Now().Add(-time.Second))
	putTestSchedule(t, n, task)
	// Run the real scheduler, not a direct model call. Completion persists the
	// dispatched item id; the worker separately executes the admitted task.
	handlers := scheduler.NewHandlerRegistry()
	_ = handlers.RegisterTask(scheduler.AgentTurnHandlerRef, n.runTaskAsAgentTurn)
	sch, err := scheduler.NewScheduler(scheduler.Config{NodeID: cfg.NodeID, MaxSleep: 10 * time.Millisecond}, n.raft, handlers)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- sch.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task = scheduleRecord(t, n, scheduleID)
		if task.LastRun != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if task.Params["last_item_id"] == "" {
		t.Fatal("scheduler did not admit visible work")
	}
	occurrence := task.Params["last_item_id"]
	// Replaying the original occurrence cannot enqueue it twice.
	replay := proto.Clone(task).(*pb.ScheduledTaskRecord)
	replay.NextRun = timestamppb.New(task.LastRun.AsTime())
	// An overlapping tick must leave the pending item as the only work.
	if err := n.dispatchBotSchedule(t.Context(), replay); err != nil {
		t.Fatal(err)
	}
	items, err := n.inboxSvc.List(t.Context(), "worker", memory.InboxFilter{})
	if err != nil || len(items) != 1 {
		t.Fatalf("overlapping work: %v %v", items, err)
	}
	if err := n.drainOneInboxItem(t.Context(), "worker"); err != nil {
		t.Fatal(err)
	}
	item, err := n.inboxSvc.Get(t.Context(), "worker", occurrence)
	if err != nil || item.Status != pb.InboxStatus_INBOX_STATUS_DONE || item.Result != "Daily outcome: checked the assigned work." {
		t.Fatalf("outcome not delivered: %+v %v", item, err)
	}
	if !strings.HasSuffix(item.Sender, ":always") {
		t.Fatal("requested notification policy lost")
	}
	if scheduleRecord(t, n, scheduleID).Params["checkpoint"] != "processed revision abc123" {
		t.Fatal("checkpoint not persisted")
	}
	stop()
	provider2 := compute.NewMockProviderFunc(func(req compute.ChatRequest, _ int) (compute.MockResponse, error) {
		var text strings.Builder
		for _, m := range req.Messages {
			text.WriteString(m.Content)
		}
		if !strings.Contains(text.String(), "processed revision abc123") || !strings.Contains(text.String(), "Daily outcome: checked") {
			t.Error("next run lost checkpoint or previous outcome")
		}
		return compute.MockResponse{Content: "No new changes."}, nil
	})
	n2, _ := bootTeamTaskNode(t, cfg, provider2, &calls, &forbidden, wire)
	task = scheduleRecord(t, n2, scheduleID)
	if err := n2.dispatchBotSchedule(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if err := n2.dispatchBotSchedule(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if err := n2.drainOneInboxItem(t.Context(), "worker"); err != nil {
		t.Fatal(err)
	}
	items, err = n2.inboxSvc.List(t.Context(), "worker", memory.InboxFilter{})
	if err != nil || len(items) != 2 {
		t.Fatalf("replay produced duplicate: %d %v", len(items), err)
	}
	update, _ := n2.builtinsRegistry.Get("schedule_update")
	if _, _, err := update(caller, map[string]string{"id": scheduleID, "enabled": "false"}); err != nil {
		t.Fatal(err)
	}
	if scheduleRecord(t, n2, scheduleID).Enabled {
		t.Fatal("pause did not persist")
	}
	other := turn.WithIdentity(t.Context(), turn.Identity{Principal: identity.Bot("other")})
	if _, _, err := update(other, map[string]string{"id": scheduleID, "enabled": "true"}); err == nil {
		t.Fatal("another agent edited the routine")
	}
}

func TestScheduledWorkRejectsChangedOwnerAndExplicitNotifyIsDurable(t *testing.T) {
	calls, forbidden := 0, 0
	n, _ := bootTeamTaskNode(t, auditTaskConfig(t), compute.NewMockProvider(), &calls, &forbidden)
	if _, err := n.botSvc.Put(t.Context(), &pb.BotRecord{Id: "worker", Owner: "user:alice", Enabled: true}, 0); err != nil {
		t.Fatal(err)
	}
	task := &pb.ScheduledTaskRecord{Id: "routine", Owner: "bot:worker", Name: "Check", Schedule: "* * * * *", NextRun: timestamppb.Now(), Params: map[string]string{"requested_by": "user:bob", "roles": "[]", "prompt": "do work"}}
	if err := n.dispatchBotSchedule(t.Context(), task); err == nil {
		t.Fatal("owner change retained old authority")
	}
	if err := (botNotifier{n: n}).Send(t.Context(), notify.Notification{BotID: "worker", UserID: "alice", Body: "I need a decision."}); err != nil {
		t.Fatal(err)
	}
	items, err := n.inboxSvc.List(t.Context(), "worker", memory.InboxFilter{})
	if err != nil || len(items) != 1 || items[0].Status != pb.InboxStatus_INBOX_STATUS_DONE || !strings.HasPrefix(items[0].Sender, "notify:") {
		t.Fatalf("notification not delivered to conversation: %v %v", items, err)
	}
	if err := (botNotifier{n: n}).Send(t.Context(), notify.Notification{BotID: "worker", UserID: "bob", Body: "private"}); err == nil {
		t.Fatal("bot notified another owner")
	}
}
