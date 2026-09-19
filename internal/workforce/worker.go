package workforce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
)

func (s *Service) holdsClaim(ctx context.Context, project, id, token string) bool {
	st, e := s.cfg.Repository.Get(ctx, project)
	if e != nil {
		return false
	}
	t := st.Tasks[id]
	x := st.Executions[id]
	return t != nil && x != nil && t.Status == StatusRunning && x.Claim == token
}

func (s *Service) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range DefaultWorkers {
		wg.Go(func() {
			ticker := time.NewTicker(PollInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.WorkOnce(ctx)
				}
			}
		})
	}
	wg.Wait()
}

func (s *Service) WorkOnce(ctx context.Context) {
	if s.cfg.Repository == nil || s.cfg.Leader != nil && !s.cfg.Leader() {
		return
	}
	states, e := s.cfg.Repository.List(ctx)
	if e != nil {
		return
	}
	for _, st := range states {
		if st.Project.Status != "active" {
			continue
		}
		for _, candidate := range st.Tasks {
			if candidate.Status == StatusRunning {
				s.expire(ctx, st.Project.Owner, st.Project.ID, candidate.ID)
				continue
			}
			if candidate.Status != StatusReady && candidate.Status != StatusPlanned {
				continue
			}
			token := ids.New()
			var task *Task
			var execution *Execution
			var project Project
			e = s.mutate(ctx, st.Project.Owner, st.Project.ID, func(current *State) error {
				t := current.Tasks[candidate.ID]
				if t.Status != StatusReady && t.Status != StatusPlanned {
					return memory.ErrClaimConflict
				}
				for _, id := range t.DependsOn {
					dep := current.Tasks[id]
					if dep != nil && (dep.Status == StatusFailed || dep.Status == StatusCancelled) {
						t.Status = StatusBlocked
						t.Question = "Dependency " + id + " did not complete; resolve it before retrying."
						bump(t)
						return nil
					}
					if dep == nil || dep.Status != StatusDone {
						return ErrBlocked
					}
				}
				t.Status = StatusRunning
				bump(t)
				x := current.Executions[t.ID]
				x.Claim = token
				x.Expires = time.Now().Add(DefaultTimeout + ClaimGrace)
				task = t
				execution = x
				project = current.Project
				return nil
			})
			if e != nil || task == nil {
				continue
			}
			s.execute(ctx, project, task, execution, token)
			return
		}
	}
}

func (s *Service) expire(ctx context.Context, owner, project, id string) {
	_ = s.mutate(ctx, owner, project, func(st *State) error {
		t := st.Tasks[id]
		x := st.Executions[id]
		if t.Status != StatusRunning || time.Now().Before(x.Expires) {
			return memory.ErrClaimConflict
		}
		t.Status = StatusBlocked
		t.Question = "Worker interrupted; reconcile external effects before retrying."
		x.Claim = ""
		bump(t)
		return nil
	})
}

func (s *Service) execute(parent context.Context, p Project, t *Task, x *Execution, token string) {
	ctx, cancel := context.WithTimeout(parent, DefaultTimeout)
	defer cancel()
	s.mu.Lock()
	s.active[t.ID] = activeWorker{token: token, cancel: cancel}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.active[t.ID].token == token {
			delete(s.active, t.ID)
		}
		s.mu.Unlock()
	}()
	if !s.holdsClaim(ctx, p.ID, t.ID, token) {
		return
	}
	var resp *turn.Response
	var err error
	if x.Approved {
		ctx = turn.WithTurnApproval(ctx, x.Action, x.Resource)
	}
	if !slices.Contains(p.BotIDs, t.AssigneeBotID) {
		err = ErrForbidden
	} else {
		err = s.bot(ctx, p.Owner, t.AssigneeBotID)
	}
	if err == nil && x.Routine != nil {
		err = s.checkRoutine(ctx, p, x.Routine)
	}
	if err == nil && x.Routine != nil && len(x.Routine.Steps) > 0 {
		err = s.executeSteps(ctx, p, t, x, token)
		if err == nil {
			resp = &turn.Response{Reply: "Completed approved routine: " + x.Routine.Name}
		}
	} else if err == nil {
		if s.cfg.Runner == nil {
			err = ErrUnavailable
		} else {
			req := turn.Request{Message: t.Instructions, Claims: x.Claims, Principal: identity.Bot(t.AssigneeBotID), TurnID: t.ID, Channel: "workforce", ChannelID: p.ID, BotID: t.AssigneeBotID, Caps: turn.BudgetCaps{MaxToolCalls: DefaultToolCalls, MaxSpendUSD: DefaultSpendUSD, MaxEgressBytes: DefaultEgressBytes}, Spent: x.Spent, RecalledContext: "<untrusted:project-context>\n" + p.Context + "\n</untrusted:project-context>\nAcceptance criteria: " + strings.Join(t.AcceptanceCriteria, "; ")}
			req.Caps.MaxToolCalls *= 1 + x.BudgetExtensions
			req.Caps.MaxSpendUSD *= float64(1 + x.BudgetExtensions)
			req.Caps.MaxEgressBytes *= int64(1 + x.BudgetExtensions)
			if x.Chat {
				st, e := s.state(ctx, p.Owner, p.ID)
				if e != nil {
					err = e
				} else if s.cfg.LoadTranscript != nil {
					for i := len(st.Messages) - 1; i >= 0; i-- {
						m := st.Messages[i]
						if m.Role == "assistant" && m.TaskID != t.ID {
							req.ConversationHistory, err = s.cfg.LoadTranscript(ctx, m.TaskID)
							break
						}
					}
				}
			}
			if err == nil {
				if len(x.History) > 0 {
					resp, err = s.cfg.Runner.Resume(ctx, req, x.History)
				} else {
					resp, err = s.cfg.Runner.Run(ctx, req)
				}
			}
		}
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil && resp == nil {
		err = fmt.Errorf("%w: runner returned no result", ErrUnavailable)
	}
	if resp != nil && !resp.NeedsConfirmation && s.cfg.SaveTranscript != nil {
		if saveErr := s.cfg.SaveTranscript(ctx, t, resp.Messages); saveErr != nil && err == nil {
			err = fmt.Errorf("persist transcript: %w", saveErr)
		}
	}
	// A cancelled turn still needs a bounded opportunity to persist its terminal state.
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(parent), ClaimGrace)
	defer finishCancel()
	_ = s.finish(finishCtx, p.ID, t.ID, token, resp, err)
}

func (s *Service) checkRoutine(ctx context.Context, p Project, r *Routine) error {
	st, e := s.state(ctx, p.Owner, p.ID)
	if e != nil {
		return e
	}
	current := st.Routines[r.ID]
	if current == nil || current.Status != "approved" || current.ApprovedDigest != digest(r) || digest(current) != current.ApprovedDigest {
		return ErrForbidden
	}
	return nil
}

func (s *Service) executeSteps(ctx context.Context, p Project, t *Task, x *Execution, token string) error {
	if s.cfg.ExecuteStep == nil || s.cfg.AuthorizeStep == nil {
		return fmt.Errorf("%w: configure browser runtime and policy", ErrUnavailable)
	}
	for i := t.Checkpoint; i < len(x.Routine.Steps); i++ {
		step := x.Routine.Steps[i]
		st, e := s.state(ctx, p.Owner, p.ID)
		if e != nil {
			return e
		}
		current := st.Tasks[t.ID]
		if st.Executions[t.ID].Claim != token || current.Status != StatusRunning {
			return memory.ErrClaimConflict
		}
		routine := st.Routines[x.Routine.ID]
		if routine == nil || routine.Status != "approved" || routine.ApprovedDigest != digest(x.Routine) || digest(routine) != routine.ApprovedDigest {
			return ErrForbidden
		}
		if step.Sensitive {
			return fmt.Errorf("%w: complete sensitive step manually", ErrBlocked)
		}
		bot, e := s.cfg.Bots.Get(ctx, t.AssigneeBotID)
		if e != nil {
			return e
		}
		if bot.GetOwner() != p.Owner || !bot.GetEnabled() || len(bot.GetTools()) > 0 && !slices.Contains(bot.GetTools(), "browser_"+step.Action) {
			return ErrForbidden
		}
		if e = s.cfg.AuthorizeStep(ctx, x.Claims, "browser_"+step.Action, p.ID); e != nil {
			return e
		}
		if e = s.cfg.ExecuteStep(ctx, p.Owner, p.ID, step); e != nil {
			return e
		}
		e = s.mutate(ctx, p.Owner, p.ID, func(st *State) error {
			if st.Executions[t.ID].Claim != token || st.Tasks[t.ID].Status != StatusRunning {
				return memory.ErrClaimConflict
			}
			st.Tasks[t.ID].Checkpoint = i + 1
			bump(st.Tasks[t.ID])
			return nil
		})
		if e != nil {
			return e
		}
	}
	return nil
}

func (s *Service) finish(ctx context.Context, project, id, token string, resp *turn.Response, runErr error) error {
	st, e := s.cfg.Repository.Get(ctx, project)
	if e != nil {
		return e
	}
	e = s.mutate(ctx, st.Project.Owner, project, func(st *State) error {
		t := st.Tasks[id]
		x := st.Executions[id]
		if t == nil || x == nil || x.Claim != token || t.Status != StatusRunning {
			return memory.ErrClaimConflict
		}
		x.Claim = ""
		x.Approved = false
		var approval *ApprovalRequired
		if errors.As(runErr, &approval) {
			resp = &turn.Response{NeedsConfirmation: true, ConfirmationAction: approval.Action, ConfirmationResource: approval.Resource, ConfirmationReason: approval.Reason}
			runErr = nil
		}
		if resp != nil {
			raw, marshalErr := json.Marshal(resp.Messages)
			if marshalErr != nil || len(raw) > MaxContinuationBytes || !validText(resp.Reply) {
				runErr = fmt.Errorf("%w: result exceeds durable task limits", ErrInvalid)
			}
		}
		switch {
		case runErr != nil:
			t.Status = StatusFailed
			t.Error = boundedText(runErr.Error())
			if errors.Is(runErr, ErrBlocked) {
				t.Status = StatusBlocked
				t.Question = boundedText(runErr.Error())
			}
		case resp == nil:
			return ErrInvalid
		case resp.NeedsConfirmation:
			t.Status = StatusApproval
			t.Question = boundedText(resp.ConfirmationReason)
			t.PromptID = ids.New()
			x.Action = resp.ConfirmationAction
			x.Resource = resp.ConfirmationResource
			x.History = resp.Messages
			x.Spent = resp.BudgetState
		default:
			x.History = nil
			t.Status = StatusDone
			t.Result = resp.Reply
			t.Question = ""
			t.PromptID = ""
			t.Artifacts = []Artifact{{ID: ids.New(), Name: "Result", Kind: "text", Reference: "/v1/tasks/" + id + "/result", CreatedAt: time.Now().UTC()}}
			if x.Chat {
				st.Messages = append(st.Messages, ProjectMessage{ID: ids.New(), ProjectID: project, Role: "assistant", SpeakerID: "bot:" + t.AssigneeBotID, Content: resp.Reply, TaskID: id, CreatedAt: time.Now().UTC()})
			}
		}
		bump(t)
		return nil
	})
	if !errors.Is(e, ErrInvalid) {
		return e
	}
	return s.mutate(ctx, st.Project.Owner, project, func(st *State) error {
		t := st.Tasks[id]
		x := st.Executions[id]
		if t == nil || x == nil || t.Status != StatusRunning || x.Claim != token {
			return memory.ErrClaimConflict
		}
		x.Claim = ""
		x.History = nil
		t.Status = StatusBlocked
		t.Question = "Result could not fit the project record; review the stored transcript before retrying."
		bump(t)
		return nil
	})
}

func boundedText(text string) string {
	if len(text) <= MaxTextBytes {
		return text
	}
	return strings.ToValidUTF8(text[:MaxTextBytes], "�")
}
