package gateway

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type captureTaskInbox struct {
	InboxAPI
	posted chan *pb.BotInboxItem
}

func (s captureTaskInbox) Post(_ context.Context, item *pb.BotInboxItem) (*pb.BotInboxItem, error) {
	s.posted <- item
	item.Id = "queued"
	item.TaskId = "linked-task"
	item.Status = pb.InboxStatus_INBOX_STATUS_WAITING
	return item, nil
}

func TestTypedInboxStampsAuthenticatedClaimsAndPreservesTaskLink(t *testing.T) {
	t.Parallel()
	inbox := captureTaskInbox{posted: make(chan *pb.BotInboxItem, 1)}
	backend := startWebREST(t, nil, func(c *RESTConfig) {
		c.Bots = &memBots{recs: map[string]*pb.BotRecord{"worker": {Id: "worker", Owner: "user:alice"}}}
		c.Inbox = inbox
	})
	client := testConsoleClient(t, backend)
	out, err := client.MutateConsole(t.Context(), &pb.MutateConsoleRequest{Identity: consoleTestIdentity(), Operation: &pb.MutateConsoleRequest_PostInbox{PostInbox: &pb.ConsoleInboxPost{Bot: "worker", Body: "work", Kind: "task"}}})
	if err != nil {
		t.Fatal(err)
	}
	item := <-inbox.posted
	if item.RequestedBy != "user:alice" || item.TaskClaims.GetUserId() != "alice" || item.TaskClaims.GetScope() != "personal" {
		t.Fatalf("caller claims not stamped: %v", item)
	}
	if out.GetInboxItem().GetTaskId() != "linked-task" {
		t.Fatalf("typed task link dropped: %v", out)
	}
}

func TestTypedBotChatReleasesStreamForDurableTaskApproval(t *testing.T) {
	t.Parallel()
	backend := startWebREST(t, &approvalConsoleRunner{}, func(c *RESTConfig) {
		c.Bots = &memBots{recs: map[string]*pb.BotRecord{"worker": {Id: "worker", Owner: "user:alice"}}}
		c.StartBotTask = func(_ context.Context, req turn.Request) (*pb.TaskApprovalRecord, error) {
			if req.Claims.UserID != "alice" || req.BotID != "worker" || req.Message != "work" {
				t.Errorf("task authority lost: %+v", req)
			}
			return &pb.TaskApprovalRecord{Id: "durable-task", State: pb.TaskApprovalState_TASK_APPROVAL_STATE_WAITING, SessionId: "bot:worker.task.durable-task", Transcript: []*pb.SessionMessage{{Role: "user", Content: "work"}, {Role: "assistant", Content: "progress"}}, Receipts: []*pb.TurnToolInvocation{{ToolName: "echo", ExecutionStatus: turn.ReceiptExecuted}, {ToolName: "delete", ExecutionStatus: turn.ReceiptApprovalRequired}}}, nil
		}
	})
	client := testConsoleClient(t, backend)
	stream, err := client.ChatConsole(t.Context(), &pb.ChatConsoleRequest{Identity: consoleTestIdentity(), Bot: "worker", Message: "/task work"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.GetNeedsConfirmation() != nil {
			t.Fatal("durable task used conversation approval loop")
		}
		if reply := event.GetReply(); reply != nil {
			found = strings.Contains(reply.Text, "durable-task") && strings.Contains(reply.Text, "/approvals")
			if len(reply.Transcript) != 2 || len(reply.Receipts) != 2 || reply.ToolCalls != 1 || len(reply.ToolsUsed) != 1 || reply.ToolsUsed[0] != "echo" || len(reply.ToolsAttempted) != 1 || reply.ToolsAttempted[0] != "delete" || reply.SessionId == "" {
				t.Fatalf("typed chat dropped or misstated evidence: %v", reply)
			}
		}
	}
	if !found {
		t.Fatal("task approval route was not delivered")
	}
}
