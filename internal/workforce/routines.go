package workforce

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

const ReviewedLiteralInput = "reviewed_literal"

var routineStructuralSelector = regexp.MustCompile(`^[a-z][a-z0-9-]*:nth-of-type\([1-9][0-9]*\)( > [a-z][a-z0-9-]*:nth-of-type\([1-9][0-9]*\))*$`)

func digest(r *Routine) string {
	raw, _ := json.Marshal(struct {
		Name, Description, Instructions, Schedule string
		Steps                                     []RoutineStep
	}{r.Name, r.Description, r.Instructions, r.Schedule, r.Steps})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func validRoutine(r *Routine) error {
	if r.Schedule != "" {
		if _, err := cron.ParseStandard(r.Schedule); err != nil {
			return fmt.Errorf("%w: invalid cron schedule: %v", ErrInvalid, err)
		}
	}
	if strings.TrimSpace(r.Name) == "" || !validText(r.Name+r.Description+r.Instructions) || len(r.Steps) > MaxSteps || r.Instructions == "" && len(r.Steps) == 0 {
		return ErrInvalid
	}
	for _, step := range r.Steps {
		if e := validRoutineStep(step); e != nil {
			return e
		}
	}
	return nil
}

func validRoutineStep(step RoutineStep) error {
	switch step.Action {
	case "navigate", "click", "fill", "press", "wait", "capture":
	default:
		return ErrInvalid
	}
	if !validText(step.Value + step.Selector + step.URL + step.Description) {
		return ErrInvalid
	}
	if step.Sensitive {
		if step.Value != "" || step.URL != "" || step.InputMode != "" || step.Selector != "" && (step.Action != "fill" || !routineStructuralSelector.MatchString(step.Selector)) {
			return fmt.Errorf("%w: sensitive steps cannot store input", ErrInvalid)
		}
		return nil
	}
	if step.Action == "fill" {
		if step.InputMode != ReviewedLiteralInput || strings.TrimSpace(step.Selector) == "" || step.Value == "" {
			return fmt.Errorf("%w: automated fill requires an explicitly reviewed literal and selector", ErrInvalid)
		}
	} else if step.InputMode != "" {
		return ErrInvalid
	}
	if step.Action == "navigate" {
		parsed, e := url.Parse(step.URL)
		if e != nil || parsed.Host == "" || parsed.Scheme != "https" && parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("%w: credential-bearing navigation must be a manual step", ErrInvalid)
		}
	}
	return nil
}
func (s *Service) CreateRoutine(ctx context.Context, owner, project string, r Routine) (*Routine, error) {
	if e := validRoutine(&r); e != nil {
		return nil, e
	}
	r.ID = ids.New()
	r.ProjectID = project
	r.Owner = owner
	r.Status = "draft"
	r.Revision = 1
	r.ApprovedDigest = ""
	r.ApprovedBy = ""
	r.CreatedAt = time.Now().UTC()
	r.UpdatedAt = r.CreatedAt
	e := s.mutate(ctx, owner, project, func(st *State) error {
		if len(st.Routines) >= MaxRecords {
			return ErrInvalid
		}
		st.Routines[r.ID] = &r
		return nil
	})
	return &r, e
}
func (s *Service) ListRoutines(ctx context.Context, owner, project string) ([]Routine, error) {
	st, e := s.state(ctx, owner, project)
	if e != nil {
		return nil, e
	}
	out := []Routine{}
	for _, r := range st.Routines {
		out = append(out, *r)
	}
	return out, nil
}
func (s *Service) ActRoutine(ctx context.Context, owner, id string, revision uint64, action string, edit Routine, claims *types.Claims) (*Routine, error) {
	st, e := s.find(ctx, owner, id, "routine")
	if e != nil {
		return nil, e
	}
	var out *Routine
	e = s.mutate(ctx, owner, st.Project.ID, func(st *State) error {
		r := st.Routines[id]
		if r.Revision != revision {
			return memory.ErrClaimConflict
		}
		switch action {
		case "approve":
			r.Status = "approved"
			r.ApprovedDigest = digest(r)
			r.ApprovedBy = owner
		case "disable":
			r.Status = "disabled"
			r.ApprovedDigest = ""
			r.ApprovedBy = ""
		case "edit":
			r.Name = edit.Name
			r.Description = edit.Description
			r.Instructions = edit.Instructions
			r.Steps = edit.Steps
			r.Schedule = edit.Schedule
			if e := validRoutine(r); e != nil {
				return e
			}
			r.Status = "draft"
			r.ApprovedDigest = ""
			r.ApprovedBy = ""
		default:
			return ErrInvalid
		}
		r.Revision++
		r.UpdatedAt = time.Now().UTC()
		out = r
		return nil
	})
	if e != nil {
		return nil, e
	}
	if out.Schedule != "" || st.Routines[id].Schedule != "" {
		if s.cfg.Schedule == nil {
			return out, fmt.Errorf("%w: scheduling unavailable", ErrUnavailable)
		}
		if e = s.cfg.Schedule(ctx, owner, out, claims); e != nil {
			return out, e
		}
	}
	return out, nil
}
func (s *Service) routineTask(ctx context.Context, st *State, r *Routine, claims *types.Claims) (*Task, error) {
	if r == nil {
		return nil, ErrNotFound
	}
	if r.Status != "approved" || r.ApprovedBy != st.Project.Owner || r.ApprovedDigest != digest(r) {
		return nil, fmt.Errorf("%w: routine requires approval of current definition", ErrForbidden)
	}
	instructions := r.Instructions
	if instructions == "" {
		instructions = "Execute the approved browser routine: " + r.Name
	}
	t, e := s.newTask(ctx, st, Task{Title: r.Name, Instructions: instructions}, claims)
	if e != nil {
		return nil, e
	}
	snapshot := *r
	st.Executions[t.ID].Routine = &snapshot
	return t, nil
}
func (s *Service) RunRoutine(ctx context.Context, owner, id string, claims *types.Claims) (*Task, error) {
	st, e := s.find(ctx, owner, id, "routine")
	if e != nil {
		return nil, e
	}
	var out *Task
	e = s.mutate(ctx, owner, st.Project.ID, func(st *State) error { var e error; out, e = s.routineTask(ctx, st, st.Routines[id], claims); return e })
	return out, e
}
func (s *Service) CreateTrigger(ctx context.Context, owner, project string, tr Trigger) (*Trigger, error) {
	tr.ID = ids.New()
	tr.ProjectID = project
	tr.Owner = owner
	tr.Revision = 1
	tr.LastFiredAt = nil
	if tr.Name == "" || !validText(tr.Name+tr.Instructions) || tr.RoutineID == "" && tr.Instructions == "" {
		return nil, ErrInvalid
	}
	e := s.mutate(ctx, owner, project, func(st *State) error {
		if len(st.Triggers) >= MaxRecords {
			return ErrInvalid
		}
		if tr.RoutineID != "" {
			r := st.Routines[tr.RoutineID]
			if r == nil {
				return ErrNotFound
			}
			if r.Status != "approved" || r.ApprovedDigest != digest(r) {
				return ErrForbidden
			}
		}
		st.Triggers[tr.ID] = &tr
		return nil
	})
	return &tr, e
}
func (s *Service) ListTriggers(ctx context.Context, owner, project string) ([]Trigger, error) {
	st, e := s.state(ctx, owner, project)
	if e != nil {
		return nil, e
	}
	out := []Trigger{}
	for _, v := range st.Triggers {
		out = append(out, *v)
	}
	return out, nil
}
func (s *Service) Fire(ctx context.Context, owner, id, event, payload string, claims *types.Claims) (*Task, bool, error) {
	if event == "" || !validText(event+payload) {
		return nil, false, ErrInvalid
	}
	st, e := s.find(ctx, owner, id, "trigger")
	if e != nil {
		return nil, false, e
	}
	var out *Task
	duplicate := false
	e = s.mutate(ctx, owner, st.Project.ID, func(st *State) error {
		key := id + ":" + event
		if taskID := st.Events[key]; taskID != "" {
			out = st.Tasks[taskID]
			duplicate = true
			return nil
		}
		if len(st.Events) >= MaxRecords {
			return ErrInvalid
		}
		tr := st.Triggers[id]
		if !tr.Enabled {
			return ErrForbidden
		}
		var e error
		if tr.RoutineID != "" {
			out, e = s.routineTask(ctx, st, st.Routines[tr.RoutineID], claims)
		} else {
			out, e = s.newTask(ctx, st, Task{Title: tr.Name, Instructions: tr.Instructions, AssigneeBotID: tr.AssigneeBotID}, claims)
		}
		if e != nil {
			return e
		}
		if payload != "" {
			out.Instructions += "\n<untrusted:event-payload>\n" + payload + "\n</untrusted:event-payload>"
		}
		st.Events[key] = out.ID
		now := time.Now().UTC()
		tr.LastFiredAt = &now
		tr.Revision++
		return nil
	})
	return out, duplicate, e
}
