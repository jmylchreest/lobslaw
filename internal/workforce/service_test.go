package workforce

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type testRepository struct {
	mu      sync.Mutex
	records map[string]*State
}

func (r *testRepository) List(context.Context) ([]*State, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []*State{}
	for _, s := range r.records {
		out = append(out, cloneState(s))
	}
	return out, nil
}
func (r *testRepository) Get(_ context.Context, id string) (*State, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.records[id]
	if s == nil {
		return nil, ErrNotFound
	}
	return cloneState(s), nil
}
func (r *testRepository) Put(_ context.Context, s *State, rev uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.records[s.Project.ID]
	if old != nil && old.Revision != rev || old == nil && rev != 0 {
		return memory.ErrClaimConflict
	}
	s.Revision = rev + 1
	r.records[s.Project.ID] = cloneState(s)
	return nil
}

type testBots struct{}

func (testBots) Get(context.Context, string) (*lobslawv1.BotRecord, error) {
	return &lobslawv1.BotRecord{Id: "worker", Owner: "user:alice", Enabled: true}, nil
}

type testRunner struct {
	calls    int
	approval bool
}

func (r *testRunner) Run(ctx context.Context, req turn.Request) (*turn.Response, error) {
	return r.RunTurn(ctx, req)
}
func (r *testRunner) Resume(ctx context.Context, req turn.Request, _ []turn.Message) (*turn.Response, error) {
	return r.RunTurn(ctx, req)
}
func (r *testRunner) RunTurn(_ context.Context, req turn.Request) (*turn.Response, error) {
	r.calls++
	if req.BotID != "worker" || req.Principal.String() != "bot:worker" || req.Caps.MaxToolCalls == 0 {
		return nil, errors.New("missing dispatch boundary")
	}
	if r.approval && r.calls == 1 {
		return &turn.Response{NeedsConfirmation: true, ConfirmationAction: "execute", ConfirmationResource: "tool:x", ConfirmationReason: "approve", Messages: []turn.Message{{Role: "user", Content: req.Message}}}, nil
	}
	return &turn.Response{Reply: "actual result"}, nil
}
func fixture(t *testing.T) (*Service, *testRepository, *testRunner, *Project) {
	t.Helper()
	r := &testRepository{records: map[string]*State{}}
	run := &testRunner{}
	s := New(Config{Repository: r, Bots: testBots{}, Runner: run})
	p, e := s.CreateProject(context.Background(), "user:alice", Project{Name: "test", CoordinatorBotID: "worker", BotIDs: []string{"worker"}})
	if e != nil {
		t.Fatal(e)
	}
	return s, r, run, p
}
func TestOwnershipAndCAS(t *testing.T) {
	t.Parallel()
	s, _, _, p := fixture(t)
	ctx := context.Background()
	for _, owner := range []string{"", "user:bob"} {
		if !errors.Is(s.AuthorizeProject(ctx, owner, p.ID), ErrForbidden) {
			t.Fatal("cross owner read")
		}
		if _, err := s.CreateTask(ctx, owner, p.ID, Task{Title: "bad", Instructions: "bad"}, nil); !errors.Is(err, ErrForbidden) {
			t.Fatal("cross owner write", err)
		}
	}
	p.Name = "updated"
	if _, err := s.UpdateProject(ctx, "user:alice", *p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateProject(ctx, "user:alice", *p); !errors.Is(err, memory.ErrClaimConflict) {
		t.Fatal("stale edit accepted", err)
	}
}
func TestDispatchDependenciesAndFencing(t *testing.T) {
	t.Parallel()
	s, _, run, p := fixture(t)
	ctx := context.Background()
	a, e := s.CreateTask(ctx, p.Owner, p.ID, Task{Title: "first", Instructions: "do work"}, &types.Claims{UserID: "alice"})
	if e != nil {
		t.Fatal(e)
	}
	b, e := s.CreateTask(ctx, p.Owner, p.ID, Task{Title: "second", Instructions: "next", DependsOn: []string{a.ID}}, &types.Claims{UserID: "alice"})
	if e != nil {
		t.Fatal(e)
	}
	if b.Status != StatusPlanned {
		t.Fatal(b.Status)
	}
	s.WorkOnce(ctx)
	a, _ = s.GetTask(ctx, p.Owner, a.ID)
	if a.Status != StatusDone || run.calls != 1 {
		t.Fatal(a, run.calls)
	}
	s.WorkOnce(ctx)
	b, _ = s.GetTask(ctx, p.Owner, b.ID)
	if b.Status != StatusDone || run.calls != 2 {
		t.Fatal(b, run.calls)
	}
	if err := s.finish(ctx, p.ID, b.ID, "stale", nil, nil); !errors.Is(err, memory.ErrClaimConflict) {
		t.Fatal("stale worker accepted", err)
	}
}
func TestEventDedupeSurvivesServiceRestart(t *testing.T) {
	t.Parallel()
	s, r, run, p := fixture(t)
	ctx := context.Background()
	tr, e := s.CreateTrigger(ctx, p.Owner, p.ID, Trigger{Name: "event", Instructions: "do work", Enabled: true})
	if e != nil {
		t.Fatal(e)
	}
	a, dup, e := s.Fire(ctx, p.Owner, tr.ID, "event-1", "data", &types.Claims{UserID: "alice"})
	if e != nil || dup {
		t.Fatal(e, dup)
	}
	s = New(Config{Repository: r, Bots: testBots{}, Runner: run})
	b, dup, e := s.Fire(ctx, p.Owner, tr.ID, "event-1", "changed", &types.Claims{UserID: "alice"})
	if e != nil || !dup || a.ID != b.ID {
		t.Fatal(e, dup, a, b)
	}
	s.WorkOnce(ctx)
	s.WorkOnce(ctx)
	if run.calls != 1 {
		t.Fatal(run.calls)
	}
}
func TestApprovalIsDurableAndNotDone(t *testing.T) {
	t.Parallel()
	s, r, run, p := fixture(t)
	run.approval = true
	ctx := context.Background()
	a, e := s.CreateTask(ctx, p.Owner, p.ID, Task{Title: "approval", Instructions: "work"}, &types.Claims{UserID: "alice"})
	if e != nil {
		t.Fatal(e)
	}
	s.WorkOnce(ctx)
	a, _ = s.GetTask(ctx, p.Owner, a.ID)
	if a.Status != StatusApproval {
		t.Fatal(a.Status)
	}
	s = New(Config{Repository: r, Bots: testBots{}, Runner: run})
	if _, e = s.ActTask(ctx, p.Owner, a.ID, a.Revision, "approve", "", nil); e != nil {
		t.Fatal(e)
	}
	s.WorkOnce(ctx)
	a, _ = s.GetTask(ctx, p.Owner, a.ID)
	if a.Status != StatusDone || run.calls != 2 {
		t.Fatal(a.Status, run.calls)
	}
}
