package node

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestDirectBotChatBudgetResumesWithoutImmediateReprompt(t *testing.T) {
	dir := t.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{NodeID: "chat-task", Functions: []types.NodeFunction{types.FunctionMemory, types.FunctionStorage}, ListenAddr: "127.0.0.1:0", DataDir: filepath.Join(dir, "data"), Bootstrap: true, SnapshotTarget: "storage:test", Creds: soulTestCreds(t, dir, "chat-task"), MemoryKey: key, Users: []config.UserConfig{{ID: "alice"}}}
	cfg.SelfLearningMode = "propose"
	provider := compute.NewMockProvider(compute.MockResponse{ToolCalls: []compute.ToolCall{{ID: "one", Name: "forbidden", Arguments: `{}`}, {ID: "two", Name: "forbidden", Arguments: `{}`}, {ID: "three", Name: "forbidden", Arguments: `{}`}}}, compute.MockResponse{Content: "completed after extension"})
	unused, calls := 0, 0
	n, _ := bootTeamTaskNode(t, cfg, provider, &unused, &calls)
	ctx := t.Context()
	_, err = n.botSvc.Put(ctx, &pb.BotRecord{Id: "worker", Owner: "user:alice", Enabled: true, Tools: []string{"forbidden"}, Budget: &pb.BotBudget{MaxToolCalls: 1}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	task, err := n.startBotChatTask(ctx, turn.Request{BotID: "worker", Claims: &types.Claims{UserID: "alice", ExpiresAt: expires}, Message: "do three calls"})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || task.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_WAITING || !task.Operation.RequiresBudgetExtension {
		t.Fatalf("budget did not pause: %v calls=%d", task, calls)
	}
	if !task.ExpiresAt.AsTime().Equal(expires) {
		t.Fatal("task outlived original caller authority")
	}
	_, err = n.taskApprovalAPI().DecideTaskApproval(ctx, &pb.DecideTaskApprovalRequest{Id: task.Id, Owner: task.Owner, Revision: task.Revision, Choice: pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_BUDGET_EXTENSION, ExtraBudget: &pb.TaskBudget{ToolCalls: 3}})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.resumeTeamTasks(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	got, err := n.taskApprovalAPI().GetTaskApproval(ctx, &pb.GetTaskApprovalRequest{Id: task.Id, Owner: task.Owner})
	if err != nil || got.Record.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_COMPLETED || calls != 3 {
		t.Fatalf("extension reprompted or replayed: %v calls=%d err=%v", got, calls, err)
	}
	// The learned review path delegates management, not authorship, to humans.
	n.policyEngine.SetDefaults([]types.PolicyRule{{ID: "review", Subject: "*", Action: "command:exec", Resource: "learned", Effect: types.EffectAllow}})
	proposal, err := n.selfTaught.Propose(ctx, &pb.SelfTaughtRecord{Kind: pb.SelfTaughtKind_SELF_TAUGHT_KIND_SKILL, Name: "worker-procedure", Body: "steps", Owner: "bot:worker"}, memory.ProposeIntent{})
	if err != nil {
		t.Fatal(err)
	}
	reviewer := learnedReviews{n}
	if _, err := reviewer.Get(ctx, &types.Claims{UserID: "bob"}, proposal.Id); err == nil {
		t.Fatal("other human read bot proposal")
	}
	view, err := reviewer.Get(ctx, &types.Claims{UserID: "alice"}, proposal.Id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reviewer.Decide(ctx, &types.Claims{UserID: "alice"}, view.ID, view.Revision, view.Digest, false); err != nil {
		t.Fatal(err)
	}
}
