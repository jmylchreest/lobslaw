package workforce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type Service struct {
	cfg    Config
	mu     sync.Mutex
	active map[string]activeWorker
}
type activeWorker struct {
	token  string
	cancel context.CancelFunc
}

func New(cfg Config) *Service { return &Service{cfg: cfg, active: map[string]activeWorker{}} }

// SetStepExecutor is a boot-time wiring hook; call before Run starts.
func (s *Service) SetStepExecutor(f func(context.Context, string, string, RoutineStep) error) {
	s.cfg.ExecuteStep = f
}

// SetAttentionSource is a boot-time hook for configured computer workspaces.
func (s *Service) SetAttentionSource(f func(context.Context, string) ([]AttentionItem, error)) {
	s.cfg.AttentionSource = f
}
func human(p string) bool {
	return strings.HasPrefix(p, "user:") && strings.TrimPrefix(p, "user:") != "" && !strings.Contains(strings.TrimPrefix(p, "user:"), ":")
}
func (s *Service) state(ctx context.Context, owner, id string) (*State, error) {
	if s == nil || s.cfg.Repository == nil {
		return nil, ErrUnavailable
	}
	st, e := s.cfg.Repository.Get(ctx, id)
	if e != nil {
		return nil, e
	}
	if !human(owner) || st.Project.Owner != owner {
		return nil, ErrForbidden
	}
	return st, nil
}
func (s *Service) AuthorizeProject(ctx context.Context, principal, projectID string) error {
	_, e := s.state(ctx, principal, projectID)
	return e
}
func (s *Service) ListProjects(ctx context.Context, owner string) ([]Project, error) {
	out := []Project{}
	if s == nil || s.cfg.Repository == nil {
		return nil, ErrUnavailable
	}
	if !human(owner) {
		return nil, ErrForbidden
	}
	all, e := s.cfg.Repository.List(ctx)
	if e != nil {
		return nil, e
	}
	for _, st := range all {
		if st.Project.Owner == owner {
			out = append(out, st.Project)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (s *Service) GetProject(ctx context.Context, owner, id string) (*Project, error) {
	st, e := s.state(ctx, owner, id)
	if e != nil {
		return nil, e
	}
	return &st.Project, nil
}
func validText(v string) bool { return len(v) <= MaxTextBytes }
func (s *Service) validateProject(ctx context.Context, p Project) error {
	if !human(p.Owner) || strings.TrimSpace(p.Name) == "" || !validText(p.Name+p.Description+p.Context) || len(p.BotIDs) > MaxRecords {
		return ErrInvalid
	}
	if p.Status != "active" && p.Status != "archived" {
		return ErrInvalid
	}
	if p.GroupID != "" {
		if s.cfg.Groups == nil {
			return ErrUnavailable
		}
		group, e := s.cfg.Groups.Get(ctx, p.GroupID)
		if e != nil {
			return e
		}
		if group.GetOwner() != p.Owner {
			return ErrForbidden
		}
	}
	if !slices.Contains(p.BotIDs, p.CoordinatorBotID) {
		return fmt.Errorf("%w: coordinator must be on roster", ErrInvalid)
	}
	for _, id := range p.BotIDs {
		if e := s.bot(ctx, p.Owner, id); e != nil {
			return e
		}
	}
	return nil
}
func (s *Service) bot(ctx context.Context, owner, id string) error {
	if s.cfg.Bots == nil {
		return ErrUnavailable
	}
	b, e := s.cfg.Bots.Get(ctx, id)
	if e != nil {
		return e
	}
	if !human(owner) || b.GetOwner() != owner {
		return ErrForbidden
	}
	if !b.GetEnabled() {
		return fmt.Errorf("%w: bot disabled", ErrInvalid)
	}
	return nil
}
func (s *Service) CreateProject(ctx context.Context, owner string, p Project) (*Project, error) {
	if s == nil || s.cfg.Repository == nil {
		return nil, ErrUnavailable
	}
	p.ID = ids.New()
	p.Owner = owner
	p.Revision = 1
	p.Status = "active"
	p.CreatedAt = time.Now().UTC()
	p.UpdatedAt = p.CreatedAt
	if e := s.validateProject(ctx, p); e != nil {
		return nil, e
	}
	st := &State{Project: p, Tasks: map[string]*Task{}, Executions: map[string]*Execution{}, Routines: map[string]*Routine{}, Triggers: map[string]*Trigger{}, Events: map[string]string{}, Messages: []ProjectMessage{}}
	if e := s.cfg.Repository.Put(ctx, st, 0); e != nil {
		return nil, e
	}
	return &p, nil
}
func (s *Service) UpdateProject(ctx context.Context, owner string, p Project) (*Project, error) {
	st, e := s.state(ctx, owner, p.ID)
	if e != nil {
		return nil, e
	}
	if p.Revision != st.Project.Revision {
		return nil, memory.ErrClaimConflict
	}
	p.Owner = owner
	p.CreatedAt = st.Project.CreatedAt
	p.UpdatedAt = time.Now().UTC()
	p.Revision++
	if e = s.validateProject(ctx, p); e != nil {
		return nil, e
	}
	st.Project = p
	if e = s.put(ctx, st); e != nil {
		return nil, e
	}
	return &p, nil
}
func (s *Service) put(ctx context.Context, st *State) error {
	raw, e := json.Marshal(st)
	if e != nil {
		return e
	}
	if len(raw) > MaxStateBytes {
		return fmt.Errorf("%w: project capacity reached", ErrInvalid)
	}
	return s.cfg.Repository.Put(ctx, st, st.Revision)
}

// mutate retries only pure state transformations. External effects never run here.
func (s *Service) mutate(ctx context.Context, owner, id string, f func(*State) error) error {
	for range MaxRetries {
		st, e := s.state(ctx, owner, id)
		if e != nil {
			return e
		}
		if e = f(st); e != nil {
			return e
		}
		e = s.put(ctx, st)
		if !errors.Is(e, memory.ErrClaimConflict) {
			return e
		}
	}
	return memory.ErrClaimConflict
}
func (s *Service) find(ctx context.Context, owner, id, kind string) (*State, error) {
	if s == nil || s.cfg.Repository == nil {
		return nil, ErrUnavailable
	}
	all, e := s.cfg.Repository.List(ctx)
	if e != nil {
		return nil, e
	}
	for _, st := range all {
		found := false
		switch kind {
		case "task":
			found = st.Tasks[id] != nil
		case "routine":
			found = st.Routines[id] != nil
		case "trigger":
			found = st.Triggers[id] != nil
		}
		if found {
			if !human(owner) || st.Project.Owner != owner {
				return nil, ErrForbidden
			}
			return st, nil
		}
	}
	return nil, ErrNotFound
}
func (s *Service) ListTasks(ctx context.Context, owner, id string) ([]Task, error) {
	st, e := s.state(ctx, owner, id)
	if e != nil {
		return nil, e
	}
	out := []Task{}
	for _, v := range st.Tasks {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (s *Service) GetTask(ctx context.Context, owner, id string) (*Task, error) {
	st, e := s.find(ctx, owner, id, "task")
	if e != nil {
		return nil, e
	}
	return st.Tasks[id], nil
}
func (s *Service) newTask(ctx context.Context, st *State, in Task, claims *types.Claims) (*Task, error) {
	retainChat(st, in.DependsOn...)
	if st.Project.Status != "active" || len(st.Tasks) >= MaxRecords || strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Instructions) == "" || !validText(in.Title+in.Instructions+strings.Join(in.AcceptanceCriteria, "")) || len(in.DependsOn) > MaxDependencies || len(in.AcceptanceCriteria) > MaxAcceptanceCriteria {
		return nil, ErrInvalid
	}
	if in.AssigneeBotID == "" {
		in.AssigneeBotID = st.Project.CoordinatorBotID
	}
	if !slices.Contains(st.Project.BotIDs, in.AssigneeBotID) {
		return nil, ErrForbidden
	}
	if e := s.bot(ctx, st.Project.Owner, in.AssigneeBotID); e != nil {
		return nil, e
	}
	status := StatusReady
	for _, id := range in.DependsOn {
		dep := st.Tasks[id]
		if dep == nil {
			return nil, fmt.Errorf("%w: unknown dependency", ErrInvalid)
		}
		if dep.Status != StatusDone {
			status = StatusPlanned
		}
	}
	now := time.Now().UTC()
	t := &Task{ID: ids.New(), ProjectID: st.Project.ID, Owner: st.Project.Owner, Title: in.Title, Instructions: in.Instructions, AssigneeBotID: in.AssigneeBotID, AcceptanceCriteria: in.AcceptanceCriteria, DependsOn: in.DependsOn, Status: status, Revision: 1, Artifacts: []Artifact{}, CreatedAt: now, UpdatedAt: now}
	if t.DependsOn == nil {
		t.DependsOn = []string{}
	}
	if t.AcceptanceCriteria == nil {
		t.AcceptanceCriteria = []string{}
	}
	st.Tasks[t.ID] = t
	st.Executions[t.ID] = &Execution{Claims: claims}
	return t, nil
}
func (s *Service) CreateTask(ctx context.Context, owner, project string, in Task, claims *types.Claims) (*Task, error) {
	var out *Task
	e := s.mutate(ctx, owner, project, func(st *State) error { var e error; out, e = s.newTask(ctx, st, in, claims); return e })
	return out, e
}
func bump(t *Task) { t.Revision++; t.UpdatedAt = time.Now().UTC() }
func (s *Service) ActTask(ctx context.Context, owner, id string, revision uint64, action, answer string, claims *types.Claims) (*Task, error) {
	st, e := s.find(ctx, owner, id, "task")
	if e != nil {
		return nil, e
	}
	var out *Task
	e = s.mutate(ctx, owner, st.Project.ID, func(st *State) error {
		t := st.Tasks[id]
		x := st.Executions[id]
		if t.Revision != revision {
			return memory.ErrClaimConflict
		}
		switch action {
		case "cancel":
			if t.Status == StatusDone {
				return ErrInvalid
			}
			t.Status = StatusCancelled
			x.Claim = ""
		case "start", "retry", "answer":
			if t.Status != StatusPlanned && t.Status != StatusBlocked && t.Status != StatusFailed {
				return ErrInvalid
			}
			if !validText(answer) {
				return ErrInvalid
			}
			if answer != "" {
				t.Instructions += "\nHuman clarification: " + answer
			}
			t.Status = StatusReady
			t.Error = ""
			t.Question = ""
			x.Claim = ""
			x.BlockedQuestion = ""
			if claims != nil {
				x.Claims = claims
			}
		case "approve":
			if t.Status != StatusApproval {
				return ErrInvalid
			}
			if x.Action == "" {
				if len(x.History) == 0 || x.BudgetExtensions >= MaxBudgetExtensions {
					return fmt.Errorf("%w: task budget exhausted; create a follow-up task", ErrInvalid)
				}
				x.BudgetExtensions++
			}
			x.Approved = true
			t.Status = StatusReady
		case "complete_step":
			if t.Status != StatusBlocked || x.Routine == nil || t.Checkpoint >= len(x.Routine.Steps) || !x.Routine.Steps[t.Checkpoint].Sensitive {
				return ErrInvalid
			}
			t.Checkpoint++
			t.Status = StatusReady
			t.Question = ""
		default:
			return ErrInvalid
		}
		bump(t)
		out = t
		return nil
	})
	if e == nil && action == "cancel" {
		s.mu.Lock()
		if worker, ok := s.active[id]; ok {
			worker.cancel()
		}
		s.mu.Unlock()
	}
	return out, e
}
func (s *Service) Attention(ctx context.Context, owner string) ([]AttentionItem, error) {
	projects, e := s.ListProjects(ctx, owner)
	if e != nil {
		return nil, e
	}
	out := []AttentionItem{}
	for _, p := range projects {
		tasks, e := s.ListTasks(ctx, owner, p.ID)
		if e != nil {
			return nil, e
		}
		for _, t := range tasks {
			kind := ""
			switch t.Status {
			case StatusApproval:
				kind = "approval"
			case StatusBlocked:
				kind = "blocked"
			case StatusFailed:
				kind = "failed"
			case StatusDone:
				kind = "deliverable"
			}
			if kind != "" {
				detail := t.Question
				if detail == "" {
					detail = t.Error
				}
				if detail == "" {
					detail = t.Result
				}
				out = append(out, AttentionItem{ID: t.ID, Kind: kind, Title: t.Title, Detail: detail, ProjectID: p.ID, TaskID: t.ID, BotID: t.AssigneeBotID, PromptID: t.PromptID, CreatedAt: t.UpdatedAt, Href: "/projects/" + p.ID})
			}
		}
	}
	if s.cfg.AttentionSource != nil {
		items, err := s.cfg.AttentionSource(ctx, owner)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if item.ProjectID != "" && s.AuthorizeProject(ctx, owner, item.ProjectID) == nil {
				out = append(out, item)
			}
		}
	}
	return out, nil
}
func (s *Service) Messages(ctx context.Context, owner, project string) ([]ProjectMessage, error) {
	st, e := s.state(ctx, owner, project)
	if e != nil {
		return nil, e
	}
	return st.Messages, nil
}
func (s *Service) Chat(ctx context.Context, owner, project, message, bot string, claims *types.Claims) (*Task, error) {
	var out *Task
	e := s.mutate(ctx, owner, project, func(st *State) error {
		var e error
		out, e = s.newTask(ctx, st, Task{Title: "Project conversation", Instructions: message, AssigneeBotID: bot}, claims)
		if e != nil {
			return e
		}
		st.Executions[out.ID].Chat = true
		for id, execution := range st.Executions {
			if id == out.ID || !execution.Chat {
				continue
			}
			prior := st.Tasks[id]
			if prior.Status == StatusReady || prior.Status == StatusRunning || prior.Status == StatusApproval || prior.Status == StatusPlanned || prior.Status == StatusBlocked {
				out.DependsOn = append(out.DependsOn, id)
				out.Status = StatusPlanned
			}
		}
		st.Messages = append(st.Messages, ProjectMessage{ID: ids.New(), ProjectID: project, Role: "user", SpeakerID: owner, Content: message, TaskID: out.ID, CreatedAt: time.Now().UTC()})
		return nil
	})
	return out, e
}
