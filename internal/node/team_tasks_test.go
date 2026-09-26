package node

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/tools"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestTeamTasksPauseRestartApproveAndResume(t *testing.T) {
	for _, delegated := range []bool{false, true} {
		name := "inbox"
		if delegated {
			name = "ask_bot"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			key, err := crypto.GenerateKey()
			if err != nil {
				t.Fatal(err)
			}
			cfg := Config{NodeID: "tasks", Functions: []types.NodeFunction{types.FunctionMemory, types.FunctionPolicy, types.FunctionStorage}, ListenAddr: "127.0.0.1:0", DataDir: filepath.Join(dir, "data"), Bootstrap: true, SnapshotTarget: "storage:test", Creds: soulTestCreds(t, dir, "tasks"), MemoryKey: key, Users: []config.UserConfig{{ID: "alice"}}}
			calls, forbidden := 0, 0
			boot := func(provider compute.LLMProvider) (*Node, func()) {
				return bootTeamTaskNode(t, cfg, provider, &calls, &forbidden)
			}
			n, stop := boot(compute.NewMockProvider(compute.MockResponse{ToolCalls: []compute.ToolCall{{ID: "effect", Name: "echo", Arguments: `{}`}}}))
			ctx := context.Background()
			worker, err := n.botSvc.Put(ctx, &pb.BotRecord{Id: "worker", Owner: "user:alice", Enabled: true, Tools: []string{"echo"}}, 0)
			if err != nil {
				t.Fatal(err)
			}
			claims := &types.Claims{UserID: "alice", Scope: "limited"}
			if delegated {
				if _, err := n.botSvc.Put(ctx, &pb.BotRecord{Id: "chief", Owner: "user:alice", Enabled: true, MayMessage: []string{"worker"}}, 0); err != nil {
					t.Fatal(err)
				}
				fn, ok := n.builtinsRegistry.Get("ask_bot")
				if !ok {
					t.Fatal("ask_bot missing")
				}
				caller := turn.WithIdentity(ctx, turn.Identity{Principal: identity.Bot("chief"), BotOwner: identity.User("alice"), BotID: "chief", UserID: "alice", Scope: "limited", OriginalClaims: claims})
				raw, code, err := fn(caller, map[string]string{"bot_id": "worker", "question": "perform the effect"})
				if err != nil || code != 0 {
					t.Fatalf("ask: %s %d %v", raw, code, err)
				}
				var result map[string]string
				if err := json.Unmarshal(raw, &result); err != nil || result["task_id"] == "" || result["status"] != "waiting" {
					t.Fatalf("no durable wait: %s %v", raw, err)
				}
			} else {
				if _, err := n.inboxSvc.Post(ctx, &pb.BotInboxItem{Recipient: "worker", RequestedBy: "user:alice", TaskClaims: turn.ClaimsToProto(claims), Body: "perform the effect"}); err != nil {
					t.Fatal(err)
				}
				if err := n.drainOneInboxItem(ctx, "worker"); err != nil {
					t.Fatal(err)
				}
			}
			item := waitingTeamTask(t, n, calls)
			// Widening the current bot does not widen this saved task.
			worker.Tools = []string{"echo", "forbidden"}
			if _, err := n.botSvc.Put(ctx, worker, worker.Revision); err != nil {
				t.Fatal(err)
			}
			stop()
			n, _ = boot(compute.NewMockProvider(compute.MockResponse{ToolCalls: []compute.ToolCall{{ID: "extra", Name: "forbidden", Arguments: `{}`}}}, compute.MockResponse{Content: "done"}))
			backend := n.taskApprovalAPI()
			got, err := backend.GetTaskApproval(ctx, &pb.GetTaskApprovalRequest{Id: item.TaskId, Owner: "user:alice"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := backend.GetTaskApproval(ctx, &pb.GetTaskApprovalRequest{Id: item.TaskId, Owner: "user:bob"}); err == nil {
				t.Fatal("other owner saw task")
			}
			if notices, err := (taskNoticeSource{n}).Notices(ctx, "user:alice"); err != nil || len(notices) != 1 {
				t.Fatalf("missing owner notice: %v %v", notices, err)
			}
			if _, err := backend.DecideTaskApproval(ctx, &pb.DecideTaskApprovalRequest{Id: item.TaskId, Owner: "user:alice", Revision: got.Record.Revision, Choice: pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_ONCE}); err != nil {
				t.Fatal(err)
			}
			assertDisabledTaskStaysReady(t, n, item)
			if err := n.resumeTeamTasks(ctx, "worker"); err != nil {
				t.Fatal(err)
			}
			if err := n.resumeTeamTasks(ctx, "worker"); err != nil {
				t.Fatal(err)
			}
			completed, err := n.inboxSvc.Get(ctx, "worker", item.Id)
			if err != nil || completed.Status != pb.InboxStatus_INBOX_STATUS_DONE || calls != 1 || forbidden != 0 {
				t.Fatalf("completion: %v %v effects=%d forbidden=%d", completed, err, calls, forbidden)
			}
		})
	}
}

func assertDisabledTaskStaysReady(t *testing.T, n *Node, item *pb.BotInboxItem) {
	t.Helper()
	ctx := t.Context()
	bot, err := n.botSvc.Get(ctx, "worker")
	if err != nil {
		t.Fatal(err)
	}
	bot.Enabled = false
	bot, err = n.botSvc.Put(ctx, bot, bot.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.resumeTeamTasks(ctx, "worker"); err == nil {
		t.Fatal("disabled bot resumed")
	}
	task, err := n.taskApprovalAPI().GetTaskApproval(ctx, &pb.GetTaskApprovalRequest{Id: item.TaskId, Owner: "user:alice"})
	if err != nil || task.Record.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_READY {
		t.Fatalf("disabled bot claimed task: %v %v", task, err)
	}
	bot.Enabled = true
	if _, err := n.botSvc.Put(ctx, bot, bot.Revision); err != nil {
		t.Fatal(err)
	}
}

func waitingTeamTask(t *testing.T, n *Node, calls int) *pb.BotInboxItem {
	t.Helper()
	ctx := t.Context()
	items, err := n.inboxSvc.List(ctx, "worker", memory.InboxFilter{})
	if err != nil || len(items) != 1 || items[0].TaskId == "" || items[0].Status != pb.InboxStatus_INBOX_STATUS_WAITING || calls != 0 {
		t.Fatalf("waiting item: %v %v calls=%d", items, err, calls)
	}
	item := items[0]
	if got, err := n.inboxSvc.Claim(ctx, "worker", "another-node"); err != nil || got != nil {
		t.Fatalf("waiting work reclaimed: %v %v", got, err)
	}
	if _, err := n.inboxSvc.Retry(ctx, "worker", item.Id); err == nil {
		t.Fatal("queue retry bypassed task approval")
	}
	return item
}

func bootTeamTaskNode(t *testing.T, cfg Config, provider compute.LLMProvider, calls, forbidden *int) (*Node, func()) {
	t.Helper()
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	n.botSvc = memory.NewBotService(n.raft, n.store)
	n.inboxSvc = memory.NewInboxService(n.raft, n.store, 0)
	n.toolRegistry = tools.NewRegistry()
	n.builtinsRegistry = tools.NewBuiltins()
	for _, name := range []string{"echo", "forbidden"} {
		if err := n.toolRegistry.Register(&types.ToolDef{Name: name, Path: compute.BuiltinScheme + name, RiskTier: types.RiskReversible}); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.builtinsRegistry.Register("echo", func(context.Context, map[string]string) ([]byte, int, error) {
		*calls++
		return []byte("effect"), 0, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := n.builtinsRegistry.Register("forbidden", func(context.Context, map[string]string) ([]byte, int, error) { *forbidden++; return nil, 0, nil }); err != nil {
		t.Fatal(err)
	}
	n.policyEngine = policy.NewEngine(n.store, n.log)
	n.policyEngine.SetDefaults([]types.PolicyRule{compute.MemoryWriteApprovalDefault(), {ID: "allow", Subject: "*", Action: "tool:exec", Resource: "*", Effect: types.EffectAllow}})
	executor := compute.NewExecutor(n.toolRegistry, n.policyEngine, nil, compute.ExecutorConfig{}, n.log)
	executor.SetBuiltins(n.builtinsRegistry)
	executor.RequireApproval("echo", "effect", compute.MemoryWriteSummary)
	n.agent, err = compute.NewAgent(compute.AgentConfig{Provider: provider, Registry: n.toolRegistry, Executor: executor, Bots: botResolverOrNil(n.botSvc)})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.wireComputeTeamsStage(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Start(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
			egress.SetActiveProvider(nil)
		})
	}
	t.Cleanup(stop)
	if err := n.raft.WaitForLeader(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	return n, stop
}
