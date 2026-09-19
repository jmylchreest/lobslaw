package workforce

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestAgentWorkRequiresOriginalClaimAndCaller(t *testing.T) {
	t.Parallel()
	s, repo, _, p := fixture(t)
	base := context.Background()
	parent, e := s.CreateTask(base, p.Owner, p.ID, Task{Title: "parent", Instructions: "delegate"}, &types.Claims{UserID: "alice", Scope: "restricted"})
	if e != nil {
		t.Fatal(e)
	}
	st, _ := repo.Get(base, p.ID)
	st.Tasks[parent.ID].Status = StatusRunning
	st.Executions[parent.ID].Claim = "claim"
	if e = repo.Put(base, st, st.Revision); e != nil {
		t.Fatal(e)
	}
	ctx := withExecution(base, p.ID, parent.ID, "claim")
	ctx = turn.WithIdentity(ctx, turn.Identity{Principal: identity.Bot("worker"), BotOwner: identity.User("alice"), BotID: "worker", TurnID: parent.ID, Channel: "workforce", ChannelID: p.ID})
	routine, e := s.CreateRoutine(base, p.Owner, p.ID, Routine{Name: "learned reporting", Instructions: "prepare report"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.AgentRunRoutine(ctx, routine.ID); !errors.Is(e, ErrForbidden) {
		t.Fatal("agent ran unapproved workflow", e)
	}
	routine, e = s.ActRoutine(base, p.Owner, routine.ID, routine.Revision, "approve", Routine{}, &types.Claims{UserID: "alice"})
	if e != nil {
		t.Fatal(e)
	}
	child, e := s.AgentCreateTask(ctx, Task{Title: "child", Instructions: "work", Owner: "user:bob", ProjectID: "elsewhere"})
	if e != nil {
		t.Fatal(e)
	}
	if child.Owner != p.Owner || child.ProjectID != p.ID || child.ParentID != parent.ID {
		t.Fatal(child)
	}
	stored, _ := repo.Get(base, p.ID)
	if stored.Executions[child.ID].Claims.Scope != "restricted" {
		t.Fatal("authority changed")
	}
	for _, bad := range []context.Context{base, turn.WithIdentity(ctx, turn.Identity{Principal: identity.Bot("other"), BotOwner: identity.User("alice"), BotID: "other", TurnID: parent.ID, Channel: "workforce", ChannelID: p.ID}), withExecution(ctx, p.ID, parent.ID, "stale")} {
		if _, e = s.AgentCreateTask(bad, Task{Title: "bad", Instructions: "bad"}); !errors.Is(e, ErrForbidden) {
			t.Fatal("forged caller accepted", e)
		}
	}
	for range MaxDelegatedTasks - 1 {
		if _, e = s.AgentCreateTask(ctx, Task{Title: "bounded child", Instructions: "work"}); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = s.AgentCreateTask(ctx, Task{Title: "overflow", Instructions: "work"}); !errors.Is(e, ErrInvalid) {
		t.Fatal("delegation budget bypass", e)
	}
	if _, e = s.AgentRunRoutine(ctx, routine.ID); !errors.Is(e, ErrInvalid) {
		t.Fatal("routine bypassed child allowance", e)
	}
}

func TestAgentCanReuseAnApprovedDemonstration(t *testing.T) {
	t.Parallel()
	s, repo, _, p := fixture(t)
	base := context.Background()
	parent, err := s.CreateTask(base, p.Owner, p.ID, Task{Title: "reuse", Instructions: "run the reporting routine"}, &types.Claims{UserID: "alice", Scope: "restricted"})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := repo.Get(base, p.ID)
	st.Tasks[parent.ID].Status = StatusRunning
	st.Executions[parent.ID].Claim = "claim"
	if err = repo.Put(base, st, st.Revision); err != nil {
		t.Fatal(err)
	}
	ctx := turn.WithIdentity(withExecution(base, p.ID, parent.ID, "claim"), turn.Identity{Principal: identity.Bot("worker"), BotOwner: identity.User("alice"), BotID: "worker", TurnID: parent.ID, Channel: "workforce", ChannelID: p.ID})
	routine, err := s.CreateRoutine(base, p.Owner, p.ID, Routine{Name: "report", Instructions: "prepare the approved report"})
	if err != nil {
		t.Fatal(err)
	}
	routine, err = s.ActRoutine(base, p.Owner, routine.ID, routine.Revision, "approve", Routine{}, &types.Claims{UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := s.AgentRoutines(ctx)
	if err != nil || len(listed) != 1 || listed[0].ID != routine.ID {
		t.Fatal("routine not discoverable", listed, err)
	}
	child, err := s.AgentRunRoutine(ctx, routine.ID)
	if err != nil || child.ParentID != parent.ID {
		t.Fatal("approved routine did not become child work", child, err)
	}
	stored, _ := repo.Get(base, p.ID)
	if stored.Executions[child.ID].Routine.ApprovedDigest != routine.ApprovedDigest || stored.Executions[child.ID].Claims.Scope != "restricted" {
		t.Fatal("workflow definition or authority changed")
	}
}

func TestDependencyContextContainsActualResults(t *testing.T) {
	t.Parallel()
	s, repo, _, p := fixture(t)
	ctx := context.Background()
	a, e := s.CreateTask(ctx, p.Owner, p.ID, Task{Title: "evidence", Instructions: "gather"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	s.WorkOnce(ctx)
	b, e := s.CreateTask(ctx, p.Owner, p.ID, Task{Title: "synthesis", Instructions: "summarize", DependsOn: []string{a.ID}, AcceptanceCriteria: []string{"CHECK-CRITERION"}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	st, _ := repo.Get(ctx, p.ID)
	st.Project.Context = "PROJECT-PIN"
	text, e := taskContext(st, b)
	if e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"PROJECT-PIN", "CHECK-CRITERION", a.ID, b.ID, "actual result", "/v1/tasks/" + a.ID + "/artifacts/", "untrusted:workforce-context"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in context", want)
		}
	}
}

func TestChatRetentionPreservesActiveDependenciesAndReceipts(t *testing.T) {
	t.Parallel()
	s, repo, _, p := fixture(t)
	transcripts := map[string]bool{}
	s.cfg.SaveTranscript = func(_ context.Context, task *Task, _ []turn.Message) error { transcripts[task.ID] = true; return nil }
	ctx := context.Background()
	first, e := s.Chat(ctx, p.Owner, p.ID, "hello", "", nil)
	if e != nil {
		t.Fatal(e)
	}
	s.WorkOnce(ctx)
	dependency, e := s.Chat(ctx, p.Owner, p.ID, "dependency evidence", "", nil)
	if e != nil {
		t.Fatal(e)
	}
	s.WorkOnce(ctx)
	held, e := s.CreateTask(ctx, p.Owner, p.ID, Task{Title: "protected", Instructions: "keep evidence", DependsOn: []string{dependency.ID}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	st, _ := repo.Get(ctx, p.ID)
	st.Tasks[held.ID].Status = StatusBlocked
	st.Events["receipt"] = first.ID
	if e = repo.Put(ctx, st, st.Revision); e != nil {
		t.Fatal(e)
	}
	for range MaxRecords + 1 {
		if _, e = s.Chat(ctx, p.Owner, p.ID, "hello", "", nil); e != nil {
			t.Fatal("chat hit permanent capacity", e)
		}
		s.WorkOnce(ctx)
	}
	st, _ = repo.Get(ctx, p.ID)
	if len(st.Messages) > MaxChatMessages || len(st.Tasks) > MaxRetainedChats+3 {
		t.Fatal("unbounded retention", len(st.Messages), len(st.Tasks))
	}
	if st.Tasks[first.ID] == nil || st.Tasks[dependency.ID] == nil || st.Tasks[held.ID] == nil || st.Events["receipt"] != first.ID {
		t.Fatal("protected state pruned")
	}
	if len(transcripts) != MaxRecords+3 {
		t.Fatal("missing archived transcripts", len(transcripts))
	}
}

type transcriptRunner struct{ response *turn.Response }

func (r transcriptRunner) Run(context.Context, turn.Request) (*turn.Response, error) {
	return r.response, nil
}
func (r transcriptRunner) Resume(context.Context, turn.Request, []turn.Message) (*turn.Response, error) {
	return r.response, nil
}
func TestArchivedChatTranscriptDoesNotConsumeContinuationCapacity(t *testing.T) {
	t.Parallel()
	s, _, _, p := fixture(t)
	saved := false
	s.cfg.SaveTranscript = func(_ context.Context, _ *Task, messages []turn.Message) error {
		saved = len(messages[0].Content) > MaxContinuationBytes
		return nil
	}
	s.cfg.Runner = transcriptRunner{response: &turn.Response{Reply: "completed conversation", Messages: []turn.Message{{Role: "user", Content: strings.Repeat("a", MaxContinuationBytes+1)}}}}
	task, e := s.Chat(context.Background(), p.Owner, p.ID, "continue", "", nil)
	if e != nil {
		t.Fatal(e)
	}
	s.WorkOnce(context.Background())
	task, e = s.GetTask(context.Background(), p.Owner, task.ID)
	if e != nil || !saved || task.Status != StatusDone {
		t.Fatal(task, e, saved)
	}
}
