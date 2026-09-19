package workforce

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"
)

type executionKey struct{}
type executionCaller struct{ project, task, claim string }

func withExecution(ctx context.Context, project, task, claim string) context.Context {
	return context.WithValue(ctx, executionKey{}, executionCaller{project, task, claim})
}

// The private claim capability comes from the worker, not a prompt, tool
// argument, principal lookup or the current row's newly issued claim token.
func authorizeExecution(ctx context.Context, st *State) (*Task, *Execution, error) {
	caller, ok := ctx.Value(executionKey{}).(executionCaller)
	id, identityOK := turn.IdentityFrom(ctx)
	if !ok || !identityOK || id.Channel != "workforce" || id.ChannelID != caller.project || id.TurnID != caller.task || id.Principal != identity.Bot(id.BotID) || id.BotID == "" || !human(id.BotOwner.String()) || id.BotOwner.String() != st.Project.Owner || st.Project.ID != caller.project || st.Project.Status != "active" {
		return nil, nil, ErrForbidden
	}
	t := st.Tasks[caller.task]
	x := st.Executions[caller.task]
	if t == nil || x == nil || t.Status != StatusRunning || x.Claim == "" || x.Claim != caller.claim || t.AssigneeBotID != id.BotID || !slices.Contains(st.Project.BotIDs, id.BotID) || x.BlockedQuestion != "" || x.Claims == nil || x.Claims.UserID == "" {
		return nil, nil, ErrForbidden
	}
	return t, x, nil
}
func (s *Service) agentState(ctx context.Context) (*State, error) {
	caller, ok := ctx.Value(executionKey{}).(executionCaller)
	if !ok {
		return nil, ErrForbidden
	}
	id, ok := turn.IdentityFrom(ctx)
	if !ok {
		return nil, ErrForbidden
	}
	st, e := s.state(ctx, id.BotOwner.String(), caller.project)
	if e != nil {
		return nil, e
	}
	if _, _, e = authorizeExecution(ctx, st); e != nil {
		return nil, e
	}
	if e = s.bot(ctx, st.Project.Owner, id.BotID); e != nil {
		return nil, e
	}
	return st, nil
}
func (s *Service) AgentProject(ctx context.Context) (*Project, error) {
	st, e := s.agentState(ctx)
	if e != nil {
		return nil, e
	}
	return &st.Project, nil
}
func (s *Service) AgentTasks(ctx context.Context) ([]Task, error) {
	st, e := s.agentState(ctx)
	if e != nil {
		return nil, e
	}
	tasks, e := s.ListTasks(ctx, st.Project.Owner, st.Project.ID)
	if e != nil {
		return nil, e
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].UpdatedAt.After(tasks[j].UpdatedAt) })
	tasks = tasks[:min(len(tasks), MaxAgentTaskList)]
	for i := range tasks {
		tasks[i].Instructions = ""
		tasks[i].AcceptanceCriteria = nil
		tasks[i].Title = preview(tasks[i].Title)
		tasks[i].Result = preview(tasks[i].Result)
		tasks[i].Progress = preview(tasks[i].Progress)
		tasks[i].Question = preview(tasks[i].Question)
		tasks[i].Error = preview(tasks[i].Error)
	}
	return tasks, nil
}
func (s *Service) AgentTask(ctx context.Context, id string) (*Task, error) {
	st, e := s.agentState(ctx)
	if e != nil {
		return nil, e
	}
	if id == "" {
		id = ctx.Value(executionKey{}).(executionCaller).task
	}
	t := st.Tasks[id]
	if t == nil {
		return nil, ErrNotFound
	}
	t.Artifacts = publicArtifacts(t)
	return t, nil
}
func (s *Service) AgentCreateTask(ctx context.Context, input Task) (*Task, error) {
	st, e := s.agentState(ctx)
	if e != nil {
		return nil, e
	}
	var out *Task
	e = s.mutate(ctx, st.Project.Owner, st.Project.ID, func(current *State) error {
		parent, x, e := authorizeExecution(ctx, current)
		if e != nil {
			return e
		}
		if x.Children >= MaxDelegatedTasks || x.Depth >= MaxDelegationDepth {
			return fmt.Errorf("%w: delegation limit reached", ErrInvalid)
		}
		out, e = s.newTask(ctx, current, input, x.Claims)
		if e != nil {
			return e
		}
		out.ParentID = parent.ID
		current.Executions[out.ID].Depth = x.Depth + 1
		x.Children++
		return nil
	})
	return out, e
}
func (s *Service) AgentCheckpoint(ctx context.Context, progress string) (*Task, error) {
	return s.agentProgress(ctx, progress, false)
}
func (s *Service) AgentBlock(ctx context.Context, question string) (*Task, error) {
	return s.agentProgress(ctx, question, true)
}
func (s *Service) agentProgress(ctx context.Context, text string, block bool) (*Task, error) {
	st, e := s.agentState(ctx)
	if e != nil {
		return nil, e
	}
	if strings.TrimSpace(text) == "" || !validText(text) {
		return nil, ErrInvalid
	}
	var out *Task
	e = s.mutate(ctx, st.Project.Owner, st.Project.ID, func(current *State) error {
		t, x, e := authorizeExecution(ctx, current)
		if e != nil {
			return e
		}
		if block {
			x.BlockedQuestion = text
			t.Question = text
		} else {
			t.Progress = text
		}
		bump(t)
		out = t
		return nil
	})
	if e == nil && block {
		caller := ctx.Value(executionKey{}).(executionCaller)
		s.mu.Lock()
		if worker, ok := s.active[caller.task]; ok && worker.token == caller.claim {
			worker.cancel()
		}
		s.mu.Unlock()
	}
	return out, e
}
