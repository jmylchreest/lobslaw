package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/internal/workforce"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type workforceAgentBots struct{}

func TestActualAgentCheckpointsAndBlocksWithoutSelfApproval(t *testing.T) {
	t.Parallel()
	answered := false
	svc, p := workforceAgentFixture(t, func(req compute.ChatRequest, i int) (compute.MockResponse, error) {
		last := req.Messages[len(req.Messages)-1].Content
		if strings.Contains(last, "OWNER-ANSWER") {
			answered = true
			if !strings.Contains(last, "VERIFIED-PROGRESS") {
				return compute.MockResponse{}, fmt.Errorf("resume lost checkpoint")
			}
			return compute.MockResponse{Content: "Finished with human answer"}, nil
		}
		switch i {
		case 0:
			return compute.MockResponse{ToolCalls: []compute.ToolCall{{ID: "checkpoint", Name: "workforce_task_checkpoint", Arguments: `{"progress":"VERIFIED-PROGRESS"}`}}}, nil
		case 1:
			return compute.MockResponse{ToolCalls: []compute.ToolCall{{ID: "block", Name: "workforce_task_block", Arguments: `{"question":"Which source should I use?"}`}}}, nil
		default:
			return compute.MockResponse{Content: "Waiting for human"}, nil
		}
	}, true)
	ctx := context.Background()
	task, e := svc.CreateTask(ctx, p.Owner, p.ID, workforce.Task{Title: "blocked", Instructions: "research sourdough starter"}, &types.Claims{UserID: "alice", Scope: "restricted"})
	if e != nil {
		t.Fatal(e)
	}
	svc.WorkOnce(ctx)
	task, e = svc.GetTask(ctx, p.Owner, task.ID)
	if e != nil || task.Status != workforce.StatusBlocked || task.Progress != "VERIFIED-PROGRESS" || task.Question != "Which source should I use?" {
		t.Fatal(task, e)
	}
	if _, e = svc.ActTask(ctx, p.Owner, task.ID, task.Revision, "answer", "OWNER-ANSWER", nil); e != nil {
		t.Fatal(e)
	}
	svc.WorkOnce(ctx)
	task, e = svc.GetTask(ctx, p.Owner, task.ID)
	if e != nil || task.Status != workforce.StatusDone || !answered {
		t.Fatal(task, e, answered)
	}
}

func (workforceAgentBots) Get(context.Context, string) (*lobslawv1.BotRecord, error) {
	return &lobslawv1.BotRecord{Id: "worker", Owner: "user:alice", Enabled: true}, nil
}
func (workforceAgentBots) ResolveBot(context.Context, string) (*compute.BotProfile, error) {
	return &compute.BotProfile{ID: "worker", Owner: "user:alice"}, nil
}

type workforceRunnerProxy struct{ inner turn.Runner }

func (r *workforceRunnerProxy) Run(ctx context.Context, req turn.Request) (*turn.Response, error) {
	return r.inner.Run(ctx, req)
}
func (r *workforceRunnerProxy) Resume(ctx context.Context, req turn.Request, history []turn.Message) (*turn.Response, error) {
	return r.inner.Resume(ctx, req, history)
}

func workforceAgentFixture(t *testing.T, script compute.ScriptFunc, allow bool) (*workforce.Service, *workforce.Project) {
	t.Helper()
	dir := t.TempDir()
	key, e := crypto.GenerateKey()
	if e != nil {
		t.Fatal(e)
	}
	store, e := memory.OpenStore(filepath.Join(dir, "state.db"), key)
	if e != nil {
		t.Fatal(e)
	}
	_, transport := raft.NewInmemTransport("workforce-tools")
	node, e := memory.NewRaft(memory.RaftConfig{NodeID: "workforce-tools", LocalAddr: "workforce-tools", DataDir: dir, Bootstrap: true, Transport: transport}, memory.NewFSM(store))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = node.Shutdown(); _ = store.Close() })
	if e = node.WaitForLeader(5 * time.Second); e != nil {
		t.Fatal(e)
	}
	record := &lobslawv1.EpisodicRecord{Id: "recall", Owner: "user:alice", Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE, Event: "sourdough starter RECALL-CONTENT", Context: "sourdough starter RECALL-CONTENT", Importance: 5, Timestamp: timestamppb.Now()}
	raw, e := proto.Marshal(&lobslawv1.LogEntry{Op: lobslawv1.LogOp_LOG_OP_PUT, Id: record.Id, Payload: &lobslawv1.LogEntry_EpisodicRecord{EpisodicRecord: record}})
	if e != nil {
		t.Fatal(e)
	}
	result, e := node.ApplyOrForward(context.Background(), raw, 5*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	if e, ok := result.(error); ok {
		t.Fatal(e)
	}
	proxy := &workforceRunnerProxy{}
	svc := workforce.New(workforce.Config{Repository: &workforce.RaftRepository{Raft: node, Store: store}, Bots: workforceAgentBots{}, Runner: proxy})
	builtins := NewBuiltins()
	if e = RegisterWorkforceBuiltins(builtins, svc); e != nil {
		t.Fatal(e)
	}
	registry := NewRegistry()
	for _, def := range WorkforceToolDefs() {
		if strings.Contains(def.Name, "approve") || strings.Contains(def.Name, "finish") {
			t.Fatal("agent authority escalation surface", def.Name)
		}
		if e = registry.Register(def); e != nil {
			t.Fatal(e)
		}
	}
	executor := compute.NewExecutor(registry, nil, nil, compute.ExecutorConfig{PolicyFallback: func(_ context.Context, claims *types.Claims, action, resource string) (policy.Decision, error) {
		if claims == nil || claims.UserID != "alice" || claims.Scope != "restricted" || action != "tool:exec" {
			return policy.Decision{Effect: types.EffectDeny}, nil
		}
		effect := types.EffectDeny
		if allow {
			effect = types.EffectAllow
		}
		return policy.Decision{Effect: effect}, nil
	}}, nil)
	executor.SetBuiltins(builtins)
	agent, e := compute.NewAgent(compute.AgentConfig{Provider: compute.NewMockProviderFunc(script), Executor: executor, Registry: registry, Bots: workforceAgentBots{}, ContextEngine: compute.NewContextEngine(compute.ContextEngineConfig{Store: store})})
	if e != nil {
		t.Fatal(e)
	}
	proxy.inner = compute.Adapt(agent)
	p, e := svc.CreateProject(context.Background(), "user:alice", workforce.Project{Name: "research", Context: "PROJECT-PIN", BotIDs: []string{"worker"}, CoordinatorBotID: "worker"})
	if e != nil {
		t.Fatal(e)
	}
	return svc, p
}

func TestActualAgentDelegatesAndReceivesDependencyResultsWithRecall(t *testing.T) {
	t.Parallel()
	calls := 0
	upstreamID := ""
	script := func(req compute.ChatRequest, index int) (compute.MockResponse, error) {
		calls++
		switch index {
		case 0:
			joined := ""
			for _, m := range req.Messages {
				if m.Role == "user" {
					joined += m.Content
				}
			}
			for _, want := range []string{"PROJECT-PIN", "RECALL-CONTENT"} {
				if !strings.Contains(joined, want) {
					return compute.MockResponse{}, fmt.Errorf("missing pinned or recalled context %q", want)
				}
			}
			return compute.MockResponse{ToolCalls: []compute.ToolCall{{ID: "create-first", Name: "workforce_task_create", Arguments: `{"title":"upstream","instructions":"gather sourdough starter evidence"}`}}}, nil
		case 1:
			last := req.Messages[len(req.Messages)-1].Content
			start, end := strings.Index(last, "{"), strings.LastIndex(last, "}")
			if start < 0 || end < start {
				return compute.MockResponse{}, fmt.Errorf("task creation failed: %s", last)
			}
			var task workforce.Task
			if e := json.Unmarshal([]byte(last[start:end+1]), &task); e != nil {
				return compute.MockResponse{}, e
			}
			upstreamID = task.ID
			args, _ := json.Marshal(map[string]any{"title": "successor", "instructions": "summarize sourdough starter evidence", "depends_on": []string{task.ID}, "acceptance_criteria": []string{"CHILD-CRITERION"}})
			return compute.MockResponse{ToolCalls: []compute.ToolCall{{ID: "create-second", Name: "workforce_task_create", Arguments: string(args)}}}, nil
		case 2:
			return compute.MockResponse{Content: "Delegated both tasks"}, nil
		case 3:
			return compute.MockResponse{Content: "ACTUAL-DEPENDENCY-EVIDENCE"}, nil
		case 4:
			last := req.Messages[len(req.Messages)-1].Content
			for _, want := range []string{"ACTUAL-DEPENDENCY-EVIDENCE", upstreamID, "/v1/tasks/" + upstreamID + "/result", "PROJECT-PIN", "RECALL-CONTENT", "CHILD-CRITERION"} {
				if !strings.Contains(last, want) {
					return compute.MockResponse{}, fmt.Errorf("successor missing %q", want)
				}
			}
			if !strings.HasSuffix(last, "summarize sourdough starter evidence") {
				return compute.MockResponse{}, fmt.Errorf("context became user question")
			}
			return compute.MockResponse{Content: "Evidence-backed synthesis"}, nil
		default:
			return compute.MockResponse{}, fmt.Errorf("unexpected extra agent call")
		}
	}
	svc, p := workforceAgentFixture(t, script, true)
	parent, e := svc.Chat(context.Background(), p.Owner, p.ID, "plan sourdough starter research", "", &types.Claims{UserID: "alice", Scope: "restricted"})
	if e != nil {
		t.Fatal(e)
	}
	for range 3 {
		svc.WorkOnce(context.Background())
	}
	tasks, e := svc.ListTasks(context.Background(), p.Owner, p.ID)
	if e != nil || len(tasks) != 3 {
		t.Fatal(tasks, e)
	}
	for _, task := range tasks {
		if task.Status != workforce.StatusDone {
			t.Fatal(task)
		}
		if task.ID != parent.ID && task.ParentID != parent.ID {
			t.Fatal("unattributed delegation", task)
		}
	}
	if calls != 5 {
		t.Fatal(calls)
	}
}

func TestWorkforceAgentToolPolicyDenialCreatesNoWork(t *testing.T) {
	t.Parallel()
	svc, p := workforceAgentFixture(t, func(_ compute.ChatRequest, i int) (compute.MockResponse, error) {
		if i == 0 {
			return compute.MockResponse{ToolCalls: []compute.ToolCall{{ID: "denied", Name: "workforce_task_create", Arguments: `{"title":"forbidden","instructions":"work"}`}}}, nil
		}
		return compute.MockResponse{Content: "Policy denied delegation"}, nil
	}, false)
	if _, e := svc.CreateTask(context.Background(), p.Owner, p.ID, workforce.Task{Title: "parent", Instructions: "plan"}, &types.Claims{UserID: "alice", Scope: "restricted"}); e != nil {
		t.Fatal(e)
	}
	svc.WorkOnce(context.Background())
	tasks, e := svc.ListTasks(context.Background(), p.Owner, p.ID)
	if e != nil || len(tasks) != 1 {
		t.Fatal(tasks, e)
	}
}
