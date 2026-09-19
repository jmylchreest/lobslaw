package workforce

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestRaftRestartPreservesEventAndRejectsStaleCAS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	key, e := crypto.GenerateKey()
	if e != nil {
		t.Fatal(e)
	}
	open := func() *RaftRepository {
		t.Helper()
		store, e := memory.OpenStore(filepath.Join(dir, "state.db"), key)
		if e != nil {
			t.Fatal(e)
		}
		_, transport := raft.NewInmemTransport("workforce-test")
		node, e := memory.NewRaft(memory.RaftConfig{NodeID: "workforce-test", LocalAddr: "workforce-test", DataDir: dir, Bootstrap: true, Transport: transport}, memory.NewFSM(store))
		if e != nil {
			t.Fatal(e)
		}
		if e = node.WaitForLeader(5 * time.Second); e != nil {
			t.Fatal(e)
		}
		return &RaftRepository{Raft: node, Store: store}
	}
	repo := open()
	s := New(Config{Repository: repo, Bots: testBots{}, Runner: &testRunner{}})
	p, e := s.CreateProject(ctx, "user:alice", Project{Name: "persistent", CoordinatorBotID: "worker", BotIDs: []string{"worker"}})
	if e != nil {
		t.Fatal(e)
	}
	stale, e := repo.Get(ctx, p.ID)
	if e != nil {
		t.Fatal(e)
	}
	trigger, e := s.CreateTrigger(ctx, p.Owner, p.ID, Trigger{Name: "event", Instructions: "work", Enabled: true})
	if e != nil {
		t.Fatal(e)
	}
	a, _, e := s.Fire(ctx, p.Owner, trigger.ID, "stable-event", "", &types.Claims{UserID: "alice"})
	if e != nil {
		t.Fatal(e)
	}
	if e = repo.Put(ctx, stale, stale.Revision); !errors.Is(e, memory.ErrClaimConflict) {
		t.Fatal("stale write accepted", e)
	}
	if e = repo.Raft.Shutdown(); e != nil {
		t.Fatal(e)
	}
	if e = repo.Store.Close(); e != nil {
		t.Fatal(e)
	}
	repo = open()
	t.Cleanup(func() { _ = repo.Raft.Shutdown(); _ = repo.Store.Close() })
	run := &testRunner{}
	s = New(Config{Repository: repo, Bots: testBots{}, Runner: run})
	b, duplicate, e := s.Fire(ctx, p.Owner, trigger.ID, "stable-event", "", &types.Claims{UserID: "alice"})
	if e != nil || !duplicate || a.ID != b.ID {
		t.Fatal(e, duplicate, a, b)
	}
	s.WorkOnce(ctx)
	s.WorkOnce(ctx)
	if run.calls != 1 {
		t.Fatal(run.calls)
	}
	task, e := s.GetTask(ctx, p.Owner, a.ID)
	if e != nil || task.Status != StatusDone {
		t.Fatal(task, e)
	}
}

func TestRoutineManualCheckpointApprovalAndEditedDigest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, _, p := fixture(t)
	calls := 0
	s.cfg.AuthorizeStep = func(context.Context, *types.Claims, string, string) error { return nil }
	s.SetStepExecutor(func(context.Context, string, string, RoutineStep) error { calls++; return nil })
	r, e := s.CreateRoutine(ctx, p.Owner, p.ID, Routine{Name: "browser", Steps: []RoutineStep{{Action: "navigate", URL: "https://example.test"}, {Action: "fill", Sensitive: true}, {Action: "capture"}}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.RunRoutine(ctx, p.Owner, r.ID, nil); !errors.Is(e, ErrForbidden) {
		t.Fatal(e)
	}
	r, e = s.ActRoutine(ctx, p.Owner, r.ID, r.Revision, "approve", Routine{}, nil)
	if e != nil {
		t.Fatal(e)
	}
	task, e := s.RunRoutine(ctx, p.Owner, r.ID, nil)
	if e != nil {
		t.Fatal(e)
	}
	s.WorkOnce(ctx)
	task, _ = s.GetTask(ctx, p.Owner, task.ID)
	if task.Status != StatusBlocked || task.Checkpoint != 1 || calls != 1 {
		t.Fatal(task, calls)
	}
	if _, e = s.ActTask(ctx, p.Owner, task.ID, task.Revision, "complete_step", "", nil); e != nil {
		t.Fatal(e)
	}
	s.WorkOnce(ctx)
	task, _ = s.GetTask(ctx, p.Owner, task.ID)
	if task.Status != StatusDone || calls != 2 {
		t.Fatal(task, calls)
	}
	r, e = s.ActRoutine(ctx, p.Owner, r.ID, r.Revision, "edit", Routine{Name: "changed", Instructions: "different"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if r.ApprovedDigest != "" || r.Status != "draft" {
		t.Fatal(r)
	}
	if _, e = s.RunRoutine(ctx, p.Owner, r.ID, nil); !errors.Is(e, ErrForbidden) {
		t.Fatal(e)
	}
}

func TestExpiredWorkBecomesBlockedWithoutRepeatingEffects(t *testing.T) {
	t.Parallel()
	s, r, run, p := fixture(t)
	ctx := context.Background()
	task, e := s.CreateTask(ctx, p.Owner, p.ID, Task{Title: "interrupted", Instructions: "do it"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	st, e := r.Get(ctx, p.ID)
	if e != nil {
		t.Fatal(e)
	}
	st.Tasks[task.ID].Status = StatusRunning
	st.Executions[task.ID].Claim = "original"
	st.Executions[task.ID].Expires = time.Now().Add(-ClaimGrace)
	if e = r.Put(ctx, st, st.Revision); e != nil {
		t.Fatal(e)
	}
	s.WorkOnce(ctx)
	got, e := s.GetTask(ctx, p.Owner, task.ID)
	if e != nil || got.Status != StatusBlocked || run.calls != 0 {
		t.Fatal(got, e, run.calls)
	}
	if e = s.finish(ctx, p.ID, task.ID, "original", nil, nil); !errors.Is(e, memory.ErrClaimConflict) {
		t.Fatal(e)
	}
}
