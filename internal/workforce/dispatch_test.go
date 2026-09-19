package workforce

import (
	"context"
	"errors"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type agentBotResolver struct{}

func (agentBotResolver) ResolveBot(context.Context, string) (*compute.BotProfile, error) {
	return &compute.BotProfile{ID: "worker", Owner: "user:alice", Tools: []string{"read_file"}, Instructions: "You are the project worker."}, nil
}
func TestTaskDispatchesThroughActualAgent(t *testing.T) {
	t.Parallel()
	s, _, _, p := fixture(t)
	provider := compute.NewMockProvider(compute.MockResponse{Content: "Agent-produced deliverable"})
	agent, e := compute.NewAgent(compute.AgentConfig{Provider: provider, Bots: agentBotResolver{}})
	if e != nil {
		t.Fatal(e)
	}
	s.cfg.Runner = compute.Adapt(agent)
	ctx := context.Background()
	task, e := s.CreateTask(ctx, p.Owner, p.ID, Task{Title: "deliver", Instructions: "Produce the requested deliverable."}, &types.Claims{UserID: "alice", Scope: "owner"})
	if e != nil {
		t.Fatal(e)
	}
	s.WorkOnce(ctx)
	task, e = s.GetTask(ctx, p.Owner, task.ID)
	if e != nil || task.Status != StatusDone || task.Result != "Agent-produced deliverable" {
		t.Fatal(task, e)
	}
}

type cancellingRunner struct {
	started chan struct{}
	release chan struct{}
}

func (r *cancellingRunner) Run(ctx context.Context, _ turn.Request) (*turn.Response, error) {
	close(r.started)
	<-r.release
	return &turn.Response{Reply: "late result after cancellation"}, ctx.Err()
}
func (r *cancellingRunner) Resume(ctx context.Context, req turn.Request, _ []turn.Message) (*turn.Response, error) {
	return r.Run(ctx, req)
}
func TestCancellationFencesLateWorker(t *testing.T) {
	t.Parallel()
	s, _, _, p := fixture(t)
	run := &cancellingRunner{started: make(chan struct{}), release: make(chan struct{})}
	s.cfg.Runner = run
	ctx := context.Background()
	task, e := s.CreateTask(ctx, p.Owner, p.ID, Task{Title: "slow", Instructions: "work"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	finished := make(chan struct{})
	go func() { s.WorkOnce(ctx); close(finished) }()
	<-run.started
	task, e = s.GetTask(ctx, p.Owner, task.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ActTask(ctx, p.Owner, task.ID, task.Revision, "cancel", "", nil); e != nil {
		t.Fatal(e)
	}
	close(run.release)
	<-finished
	task, e = s.GetTask(ctx, p.Owner, task.ID)
	if e != nil || task.Status != StatusCancelled || task.Result != "" {
		t.Fatal(task, e)
	}
}

func TestScheduledOccurrenceDedupeAndCrossOwnerSurfaces(t *testing.T) {
	t.Parallel()
	s, _, _, p := fixture(t)
	ctx := context.Background()
	r, e := s.CreateRoutine(ctx, p.Owner, p.ID, Routine{Name: "scheduled", Instructions: "work"})
	if e != nil {
		t.Fatal(e)
	}
	r, e = s.ActRoutine(ctx, p.Owner, r.ID, r.Revision, "approve", Routine{}, nil)
	if e != nil {
		t.Fatal(e)
	}
	for range 2 {
		if e = s.ScheduledRun(ctx, p.Owner, r.ID, "2026-09-19T12:00:00Z", r.ApprovedDigest, nil); e != nil {
			t.Fatal(e)
		}
	}
	tasks, e := s.ListTasks(ctx, p.Owner, p.ID)
	if e != nil || len(tasks) != 1 {
		t.Fatal(e, tasks)
	}
	for _, owner := range []string{"", "user:bob"} {
		if _, e = s.GetTask(ctx, owner, tasks[0].ID); !errors.Is(e, ErrForbidden) {
			t.Fatal(e)
		}
		if _, e = s.ActRoutine(ctx, owner, r.ID, r.Revision, "disable", Routine{}, nil); !errors.Is(e, ErrForbidden) {
			t.Fatal(e)
		}
		if _, e = s.Messages(ctx, owner, p.ID); !errors.Is(e, ErrForbidden) {
			t.Fatal(e)
		}
		if _, e = s.ListTriggers(ctx, owner, p.ID); !errors.Is(e, ErrForbidden) {
			t.Fatal(e)
		}
	}
	attention, e := s.Attention(ctx, "user:bob")
	if e != nil || len(attention) != 0 {
		t.Fatal(attention, e)
	}
}
