package console

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type PlanService interface {
	GetPlan(ctx context.Context, req *lobslawv1.GetPlanRequest) (*lobslawv1.GetPlanResponse, error)
}
type PlanView struct {
	WindowSeconds  float64             `json:"window_seconds"`
	Commitments    []CommitmentView    `json:"commitments,omitempty"`
	ScheduledTasks []ScheduledTaskView `json:"scheduled_tasks,omitempty"`
}

type CommitmentView struct {
	ID     string    `json:"id"`
	DueAt  time.Time `json:"due_at"`
	Reason string    `json:"reason,omitempty"`
	Status string    `json:"status"`
}

type ScheduledTaskView struct {
	ID         string    `json:"id"`
	Name       string    `json:"name,omitempty"`
	Schedule   string    `json:"schedule"`
	HandlerRef string    `json:"handler_ref"`
	NextRun    time.Time `json:"next_run,omitempty"`
}

func (s *Service) Plan(ctx context.Context, claims *types.Claims, window string) (PlanView, error) {
	if _, err := caller(ctx, claims); err != nil {
		return PlanView{}, err
	}
	if s.cfg.Plan == nil {
		return PlanView{}, ErrUnavailable
	}
	req := &lobslawv1.GetPlanRequest{}
	if wq := window; wq != "" {
		if d, err := time.ParseDuration(wq); err == nil && d > 0 {
			req.Window = durationpb.New(d)
		}
	}
	resp, err := s.cfg.Plan.GetPlan(ctx, req)
	if err != nil {
		return PlanView{}, err
	}

	out := PlanView{
		WindowSeconds: resp.Window.AsDuration().Seconds(),
	}
	for _, c := range resp.Commitments {
		out.Commitments = append(out.Commitments, CommitmentView{
			ID:     c.Id,
			DueAt:  c.DueAt.AsTime(),
			Reason: c.Reason,
			Status: c.Status,
		})
	}
	for _, t := range resp.ScheduledTasks {
		entry := ScheduledTaskView{
			ID:         t.Id,
			Name:       t.Name,
			Schedule:   t.Schedule,
			HandlerRef: t.HandlerRef,
		}
		if t.NextRun != nil {
			entry.NextRun = t.NextRun.AsTime()
		}
		out.ScheduledTasks = append(out.ScheduledTasks, entry)
	}

	return out, nil
}
