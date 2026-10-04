package node

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestConversationCreatesTaskOnlyWhenModelChooses(t *testing.T) {
	provider := compute.NewMockProviderFunc(func(req compute.ChatRequest, index int) (compute.MockResponse, error) {
		switch index {
		case 0:
			return compute.MockResponse{Content: "Hello!"}, nil
		case 1:
			return compute.MockResponse{ToolCalls: []compute.ToolCall{{ID: "queue", Name: "task_create", Arguments: `{"subject":"Backup plan","body":"Prepare a backup plan for the staging server."}`}}}, nil
		case 2:
			return compute.MockResponse{Content: "I queued the backup plan."}, nil
		case 3:
			for _, tool := range req.Tools {
				if tool.Name == "task_create" {
					t.Error("queued task can recursively create itself")
				}
			}
			for _, message := range req.Messages {
				if strings.Contains(message.Content, "PRIVATE-CHAT") {
					t.Error("queued task inherited unrelated conversation history")
				}
			}
			return compute.MockResponse{Content: "Backup plan complete."}, nil
		default:
			return compute.MockResponse{}, errors.New("unexpected model call")
		}
	})
	calls, forbidden := 0, 0
	n, _ := bootTeamTaskNode(t, auditTaskConfig(t), provider, &calls, &forbidden)
	ctx := t.Context()
	if _, err := n.botSvc.Put(ctx, &pb.BotRecord{Id: "worker", Owner: "user:alice", Enabled: true}, 0); err != nil {
		t.Fatal(err)
	}
	request := turn.Request{BotID: "worker", Principal: identity.Bot("worker"), Claims: &types.Claims{UserID: "alice"}, Message: "Hello"}
	reply, err := n.agent.Run(ctx, request)
	if err != nil || reply.Reply != "Hello!" {
		t.Fatalf("greeting failed: %v, %v", reply, err)
	}
	items, err := n.inboxSvc.List(ctx, "worker", memory.InboxFilter{})
	if err != nil || len(items) != 0 {
		t.Fatalf("greeting created work: %v, %v", items, err)
	}
	request.Message = "Create a task to prepare a backup plan for staging."
	request.ConversationHistory = []turn.Message{{Role: "user", Content: "PRIVATE-CHAT"}}
	reply, err = n.agent.Run(ctx, request)
	if err != nil || len(reply.ToolCalls) != 1 || reply.ToolCalls[0].ExecutionStatus != turn.ReceiptExecuted {
		t.Fatalf("model did not queue work: %v, %v", reply, err)
	}
	items, err = n.inboxSvc.List(ctx, "worker", memory.InboxFilter{})
	if err != nil || len(items) != 1 || items[0].Status != pb.InboxStatus_INBOX_STATUS_PENDING || items[0].Kind != pb.InboxKind_INBOX_KIND_TASK {
		t.Fatalf("missing queued task: %v, %v", items, err)
	}
	if err := n.drainOneInboxItem(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	item, err := n.inboxSvc.Get(ctx, "worker", items[0].Id)
	if err != nil || item.Status != pb.InboxStatus_INBOX_STATUS_DONE || item.TaskId == "" || item.Result != "Backup plan complete." {
		t.Fatalf("queued task did not complete: %v, %v", item, err)
	}
}

func TestInterruptedTaskIsNotReportedCompleted(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	provider := compute.NewMockProviderFunc(func(compute.ChatRequest, int) (compute.MockResponse, error) {
		cancel()
		return compute.MockResponse{}, context.Canceled
	})
	calls, forbidden := 0, 0
	n, _ := bootTeamTaskNode(t, auditTaskConfig(t), provider, &calls, &forbidden)
	if _, err := n.botSvc.Put(t.Context(), &pb.BotRecord{Id: "worker", Owner: "user:alice", Enabled: true}, 0); err != nil {
		t.Fatal(err)
	}
	bot, err := botResolverOrNil(n.botSvc).ResolveBot(t.Context(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	response, err := n.startTeamTask(ctx, compute.ProcessMessageRequest{Bot: bot, BotID: "worker", Claims: &types.Claims{UserID: "alice"}, Message: "Do assigned work"}, nil)
	if !errors.Is(err, context.Canceled) || response == nil || response.TaskID == "" {
		t.Fatalf("interruption was swallowed: %v, %v", response, err)
	}
	task, err := n.taskApprovalAPI().GetTaskApproval(t.Context(), &pb.GetTaskApprovalRequest{Id: response.TaskID, Owner: "user:alice"})
	if err != nil || task.Record.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_OUTCOME_UNKNOWN {
		t.Fatalf("interrupted task recorded as success: %v, %v", task, err)
	}
	if len(provider.Calls()) != 1 || len(task.Record.Transcript) == 0 {
		t.Fatal("task lost evidence or made an unnecessary summary call")
	}
}
