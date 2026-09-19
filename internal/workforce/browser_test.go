package workforce

import (
	"context"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type restrictedBots struct{}

func (restrictedBots) Get(context.Context, string) (*lobslawv1.BotRecord, error) {
	return &lobslawv1.BotRecord{Id: "worker", Owner: "user:alice", Enabled: true, Tools: []string{"read_file"}}, nil
}

func TestBrowserPolicyApprovalSurvivesRestartAndBotFilterWins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, repo, _, p := fixture(t)
	executed := 0
	s.cfg.AuthorizeStep = func(ctx context.Context, _ *types.Claims, action, _ string) error {
		if turn.Approved(ctx, "tool:exec", action) {
			return nil
		}
		return &ApprovalRequired{Action: "tool:exec", Resource: action, Reason: "Browser requires approval"}
	}
	s.SetStepExecutor(func(context.Context, string, string, RoutineStep) error { executed++; return nil })
	r, e := s.CreateRoutine(ctx, p.Owner, p.ID, Routine{Name: "reviewed", Steps: []RoutineStep{{Action: "navigate", URL: "https://example.test"}}})
	if e != nil {
		t.Fatal(e)
	}
	r, e = s.ActRoutine(ctx, p.Owner, r.ID, r.Revision, "approve", Routine{}, nil)
	if e != nil {
		t.Fatal(e)
	}
	task, e := s.RunRoutine(ctx, p.Owner, r.ID, &types.Claims{UserID: "alice"})
	if e != nil {
		t.Fatal(e)
	}
	s.WorkOnce(ctx)
	task, _ = s.GetTask(ctx, p.Owner, task.ID)
	if task.Status != StatusApproval || executed != 0 {
		t.Fatal(task, executed)
	}
	cfg := s.cfg
	cfg.Repository = repo
	s = New(cfg)
	if _, e = s.ActTask(ctx, p.Owner, task.ID, task.Revision, "approve", "", nil); e != nil {
		t.Fatal(e)
	}
	s.WorkOnce(ctx)
	task, _ = s.GetTask(ctx, p.Owner, task.ID)
	if task.Status != StatusDone || executed != 1 {
		t.Fatal(task, executed)
	}
	s.cfg.Bots = restrictedBots{}
	task, e = s.RunRoutine(ctx, p.Owner, r.ID, &types.Claims{UserID: "alice"})
	if e != nil {
		t.Fatal(e)
	}
	s.WorkOnce(ctx)
	task, _ = s.GetTask(ctx, p.Owner, task.ID)
	if task.Status != StatusFailed || executed != 1 {
		t.Fatal("bot allowlist bypass", task, executed)
	}
}

func TestMissingBrowserNeverCompletesRoutine(t *testing.T) {
	t.Parallel()
	s, _, _, p := fixture(t)
	ctx := context.Background()
	r, e := s.CreateRoutine(ctx, p.Owner, p.ID, Routine{Name: "unconfigured", Steps: []RoutineStep{{Action: "capture"}}})
	if e != nil {
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
	if task.Status != StatusFailed || task.Result != "" {
		t.Fatal(task)
	}
}
