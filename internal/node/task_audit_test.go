package node

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func auditTaskConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return Config{NodeID: "audit-task", Functions: []types.NodeFunction{types.FunctionMemory, types.FunctionStorage}, ListenAddr: "127.0.0.1:0", DataDir: filepath.Join(dir, "data"), Bootstrap: true, SnapshotTarget: "storage:test", Creds: soulTestCreds(t, dir, "audit-task"), MemoryKey: key, Users: []config.UserConfig{{ID: "alice"}}}
}

func TestInitialTaskFailureRetainsEvidenceAndReleasesCapacity(t *testing.T) {
	provider := compute.NewMockProvider(compute.MockResponse{ToolCalls: []compute.ToolCall{{ID: "effect", Name: "forbidden", Arguments: `{}`}}}, compute.MockResponse{Err: errors.New("provider failed after effect")}, compute.MockResponse{Content: "next task"})
	echo, calls := 0, 0
	n, _ := bootTeamTaskNode(t, auditTaskConfig(t), provider, &echo, &calls, func(n *Node, _ *compute.AgentConfig) { n.inboxSvc = memory.NewInboxService(n.raft, n.store, 1) })
	ctx := t.Context()
	if _, err := n.botSvc.Put(ctx, &pb.BotRecord{Id: "worker", Owner: "user:alice", Enabled: true}, 0); err != nil {
		t.Fatal(err)
	}
	request := turn.Request{BotID: "worker", Claims: &types.Claims{UserID: "alice"}, Message: "perform effect"}
	task, err := n.startBotChatTask(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_OUTCOME_UNKNOWN || task.Recoverable || calls != 1 {
		t.Fatalf("failure not fenced: %v calls=%d", task, calls)
	}
	if len(task.Transcript) < 3 || len(task.Receipts) != 1 || task.Receipts[0].ExecutionStatus != turn.ReceiptExecuted {
		t.Fatalf("lost observed effect: %v", task)
	}
	items, err := n.inboxSvc.List(ctx, "worker", memory.InboxFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Status != pb.InboxStatus_INBOX_STATUS_FAILED {
		t.Fatal("uncertain work remained waiting")
	}
	if _, err := n.startBotChatTask(ctx, request); err != nil {
		t.Fatalf("capacity not released: %v", err)
	}
	if calls != 1 {
		t.Fatal("uncertain effect replayed")
	}
	closed, err := n.taskApprovalAPI().CancelTaskApproval(ctx, &pb.CancelTaskApprovalRequest{Id: task.Id, Owner: task.Owner, Revision: task.Revision})
	if err != nil || closed.Record.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_CANCELLED {
		t.Fatalf("owner closure: %v %v", closed, err)
	}
	history, err := n.newSessionBrowser().LoadMessages(ctx, task.SessionId)
	if err != nil || len(history) < len(task.Transcript)+1 {
		t.Fatalf("durable task history missing: %v %v", history, err)
	}
}

func TestCoordinatorTaskPreservesOwnerContextHistoryAndDelegation(t *testing.T) {
	provider := compute.NewMockProviderFunc(func(req compute.ChatRequest, index int) (compute.MockResponse, error) {
		var text strings.Builder
		for _, m := range req.Messages {
			text.WriteString(m.Content)
		}
		hasAsk, hasMemory := false, false
		for _, tool := range req.Tools {
			hasAsk = hasAsk || tool.Name == "ask_bot"
			hasMemory = hasMemory || tool.Name == "memory_search"
		}
		switch index {
		case 0:
			if !strings.Contains(text.String(), "OWNER-DIARY") || !strings.Contains(text.String(), "SUPPLIED-HISTORY") || !hasAsk || !hasMemory {
				t.Errorf("coordinator lost context or tools: ask=%v memory=%v %s", hasAsk, hasMemory, text.String())
			}
			return compute.MockResponse{ToolCalls: []compute.ToolCall{{ID: "delegate", Name: "ask_bot", Arguments: `{"bot_id":"worker","question":"handle assigned work","coordinator_conversation":"true"}`}}}, nil
		case 1, 4:
			if strings.Contains(text.String(), "OWNER-DIARY") || strings.Contains(text.String(), "SUPPLIED-HISTORY") || hasAsk || hasMemory {
				t.Error("specialist inherited coordinator context or classification")
			}
			return compute.MockResponse{Content: "child-finished"}, nil
		case 2:
			return compute.MockResponse{Content: "coordinator-finished"}, nil
		case 3:
			if !strings.Contains(text.String(), "coordinator-finished") {
				t.Error("next coordinator turn lost durable full history")
			}
			return compute.MockResponse{Content: "second-coordinator-turn"}, nil
		default:
			return compute.MockResponse{}, errors.New("unexpected replay")
		}
	})
	echo, calls := 0, 0
	n, _ := bootTeamTaskNode(t, auditTaskConfig(t), provider, &echo, &calls, func(n *Node, c *compute.AgentConfig) {
		c.ContextEngine = compute.NewContextEngine(compute.ContextEngineConfig{Store: n.store})
		c.Soul = func() *types.SoulConfig { return &types.SoulConfig{} }
		if err := n.toolRegistry.Register(&types.ToolDef{Name: "memory_search", Path: compute.BuiltinScheme + "memory_search", RiskTier: types.RiskReversible}); err != nil {
			t.Fatal(err)
		}
	})
	ctx := t.Context()
	for _, bot := range []*pb.BotRecord{{Id: "worker", Owner: "user:alice", Enabled: true}, {Id: "chief", Owner: "user:alice", Enabled: true, IsCoordinator: true, MayMessage: []string{"worker"}}} {
		if _, err := n.botSvc.Put(ctx, bot, 0); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := proto.Marshal(&pb.LogEntry{Op: pb.LogOp_LOG_OP_PUT, Id: "diary", Payload: &pb.LogEntry_EpisodicRecord{EpisodicRecord: &pb.EpisodicRecord{Id: "diary", Owner: "user:alice", Visibility: pb.Visibility_VISIBILITY_PRIVATE, Event: "sourdough OWNER-DIARY fed Tuesdays", Context: "sourdough OWNER-DIARY fed Tuesdays", Importance: 5, Timestamp: timestamppb.Now()}}})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := n.raft.Apply(raw, time.Second); err != nil {
		t.Fatal(err)
	} else if e, ok := result.(error); ok {
		t.Fatal(e)
	}
	request := turn.Request{BotID: "chief", Claims: &types.Claims{UserID: "alice"}, Message: "sourdough plan", ConversationHistory: []turn.Message{{Role: "user", Content: "SUPPLIED-HISTORY"}}}
	for range 2 {
		task, err := n.startBotChatTask(ctx, request)
		if err != nil || task.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_COMPLETED {
			t.Fatalf("coordinator task: %v %v", task, err)
		}
	}
	request.BotID = "worker"
	if _, err := n.startBotChatTask(ctx, request); err != nil {
		t.Fatal(err)
	}
	if len(provider.Calls()) != 5 {
		t.Fatalf("delegation did not execute: %d calls", len(provider.Calls()))
	}
}

func TestCoordinatorCheckpointRefusesDemotedActor(t *testing.T) {
	provider := compute.NewMockProvider(compute.MockResponse{ToolCalls: []compute.ToolCall{{ID: "pause", Name: "echo", Arguments: `{}`}}})
	calls, unused := 0, 0
	n, _ := bootTeamTaskNode(t, auditTaskConfig(t), provider, &calls, &unused)
	ctx := t.Context()
	bot, err := n.botSvc.Put(ctx, &pb.BotRecord{Id: "chief", Owner: "user:alice", Enabled: true, IsCoordinator: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	task, err := n.startBotChatTask(ctx, turn.Request{BotID: "chief", Claims: &types.Claims{UserID: "alice"}, Message: "write", ConversationHistory: []turn.Message{{Role: "user", Content: "private owner context"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !task.CoordinatorConversation || task.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_WAITING {
		t.Fatalf("not a paused coordinator: %v", task)
	}
	if _, err := n.taskApprovalAPI().DecideTaskApproval(ctx, &pb.DecideTaskApprovalRequest{Id: task.Id, Owner: task.Owner, Revision: task.Revision, Choice: pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_ONCE}); err != nil {
		t.Fatal(err)
	}
	bot.IsCoordinator = false
	if _, err := n.botSvc.Put(ctx, bot, bot.Revision); err != nil {
		t.Fatal(err)
	}
	if err := n.resumeTeamTasks(ctx, "chief"); err == nil {
		t.Fatal("demoted actor resumed owner context")
	}
	if len(provider.Calls()) != 1 || calls != 0 {
		t.Fatal("demoted actor reached model or tool")
	}
}
