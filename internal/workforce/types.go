// Package workforce provides owned, durable project work above the turn boundary.
package workforce

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

const (
	StatusPlanned                           = "planned"
	StatusReady                             = "ready"
	StatusRunning                           = "running"
	StatusBlocked                           = "blocked"
	StatusApproval                          = "needs_approval"
	StatusDone                              = "done"
	StatusFailed                            = "failed"
	StatusCancelled                         = "cancelled"
	DefaultWorkers            int           = 2
	MaxBudgetExtensions       int           = 1
	MaxDelegatedTasks         int           = 8
	MaxDelegationDepth        int           = 4
	MaxRetainedChats          int           = 32
	MaxChatMessages           int           = 64
	MaxDependencyResultBytes  int           = 4096
	MaxDependencyContextBytes int           = 32 << 10
	MaxTaskContextBytes       int           = 128 << 10
	MaxDependencies           int           = 32
	MaxAcceptanceCriteria     int           = 32
	MaxAgentTaskList          int           = 64
	MaxAgentPreviewBytes      int           = 512
	DefaultTimeout            time.Duration = 2 * time.Minute
	PollInterval              time.Duration = time.Second
	ClaimGrace                time.Duration = 30 * time.Second
	MaxRecords                int           = 256
	MaxStateBytes             int           = 4 << 20
	MaxTextBytes              int           = 32 << 10
	MaxContinuationBytes      int           = 512 << 10
	MaxSteps                  int           = 64
	MaxRetries                int           = 8
	DefaultToolCalls          int           = 24
	DefaultSpendUSD           float64       = 1
	DefaultEgressBytes        int64         = 16 << 20
)

var (
	ErrForbidden   = errors.New("workforce: project is not owned by this account")
	ErrNotFound    = errors.New("workforce: record not found")
	ErrUnavailable = errors.New("workforce: infrastructure unavailable")
	ErrInvalid     = errors.New("workforce: invalid request")
	ErrBlocked     = errors.New("workforce: human intervention required")
)

type ApprovalRequired struct{ Action, Resource, Reason string }

func (e *ApprovalRequired) Error() string { return e.Reason }

type Project struct {
	ID               string    `json:"id"`
	Owner            string    `json:"owner"`
	GroupID          string    `json:"group_id,omitempty"`
	Name             string    `json:"name"`
	Description      string    `json:"description"`
	CoordinatorBotID string    `json:"coordinator_bot_id"`
	BotIDs           []string  `json:"bot_ids"`
	Context          string    `json:"context"`
	Status           string    `json:"status"`
	Revision         uint64    `json:"revision"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}
type Artifact struct {
	MimeType  string    `json:"mime_type,omitempty"`
	Size      int64     `json:"size,omitempty"`
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Reference string    `json:"reference"`
	CreatedAt time.Time `json:"created_at"`
}
type Task struct {
	Acknowledged       bool       `json:"acknowledged"`
	ParentID           string     `json:"parent_id,omitempty"`
	Progress           string     `json:"progress,omitempty"`
	ID                 string     `json:"id"`
	ProjectID          string     `json:"project_id"`
	Owner              string     `json:"owner"`
	Title              string     `json:"title"`
	Instructions       string     `json:"instructions"`
	AssigneeBotID      string     `json:"assignee_bot_id"`
	AcceptanceCriteria []string   `json:"acceptance_criteria"`
	DependsOn          []string   `json:"depends_on"`
	Status             string     `json:"status"`
	Revision           uint64     `json:"revision"`
	Checkpoint         int        `json:"checkpoint,omitempty"`
	Result             string     `json:"result,omitempty"`
	Error              string     `json:"error,omitempty"`
	Question           string     `json:"question,omitempty"`
	PromptID           string     `json:"prompt_id,omitempty"`
	Artifacts          []Artifact `json:"artifacts"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}
type ProjectMessage struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Role      string    `json:"role"`
	SpeakerID string    `json:"speaker_id"`
	Content   string    `json:"content"`
	TaskID    string    `json:"task_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}
type RoutineStep struct {
	InputMode   string `json:"input_mode,omitempty"`
	Action      string `json:"action"`
	Selector    string `json:"selector,omitempty"`
	Value       string `json:"value,omitempty"`
	URL         string `json:"url,omitempty"`
	Description string `json:"description,omitempty"`
	Sensitive   bool   `json:"sensitive,omitempty"`
}
type Routine struct {
	ID             string        `json:"id"`
	ProjectID      string        `json:"project_id"`
	Owner          string        `json:"owner"`
	Name           string        `json:"name"`
	Description    string        `json:"description"`
	Instructions   string        `json:"instructions"`
	Steps          []RoutineStep `json:"steps"`
	Status         string        `json:"status"`
	Revision       uint64        `json:"revision"`
	ApprovedDigest string        `json:"approved_digest,omitempty"`
	ApprovedBy     string        `json:"approved_by,omitempty"`
	Schedule       string        `json:"schedule,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
}
type Trigger struct {
	ID            string     `json:"id"`
	ProjectID     string     `json:"project_id"`
	Owner         string     `json:"owner"`
	Name          string     `json:"name"`
	RoutineID     string     `json:"routine_id,omitempty"`
	Instructions  string     `json:"instructions,omitempty"`
	AssigneeBotID string     `json:"assignee_bot_id,omitempty"`
	Enabled       bool       `json:"enabled"`
	Revision      uint64     `json:"revision"`
	LastFiredAt   *time.Time `json:"last_fired_at,omitempty"`
}
type AttentionItem struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Detail    string    `json:"detail"`
	ProjectID string    `json:"project_id"`
	TaskID    string    `json:"task_id"`
	BotID     string    `json:"bot_id"`
	PromptID  string    `json:"prompt_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Href      string    `json:"href"`
}

// Execution fields are never decoded from caller-supplied task JSON.
type Execution struct {
	ArtifactReferences map[string]string
	Children           int
	Depth              int
	BlockedQuestion    string
	WorkHistory        []turn.Message
	TranscriptSaved    bool
	Claim              string
	Expires            time.Time
	Claims             *types.Claims
	History            []turn.Message
	Spent              turn.BudgetState
	Action             string
	Resource           string
	Approved           bool
	BudgetExtensions   int
	Routine            *Routine
	Chat               bool
}
type State struct {
	Revision   uint64
	Project    Project
	Tasks      map[string]*Task
	Executions map[string]*Execution
	Routines   map[string]*Routine
	Triggers   map[string]*Trigger
	Events     map[string]string
	Messages   []ProjectMessage
}
type Repository interface {
	List(context.Context) ([]*State, error)
	Get(context.Context, string) (*State, error)
	Put(context.Context, *State, uint64) error
}
type BotReader interface {
	Get(context.Context, string) (*lobslawv1.BotRecord, error)
}
type GroupReader interface {
	Get(context.Context, string) (*lobslawv1.GroupRecord, error)
}
type Config struct {
	OpenArtifact    func(string) (io.ReadCloser, error)
	Repository      Repository
	Bots            BotReader
	Groups          GroupReader
	Runner          turn.Runner
	ExecuteStep     func(context.Context, string, string, RoutineStep) error
	AuthorizeStep   func(context.Context, *types.Claims, string, string) error
	Schedule        func(context.Context, string, *Routine, *types.Claims) error
	SaveTranscript  func(context.Context, *Task, []turn.Message) error
	LoadTranscript  func(context.Context, string) ([]turn.Message, error)
	AttentionSource func(context.Context, string) ([]AttentionItem, error)
	Leader          func() bool
}

func cloneState(s *State) *State {
	raw, _ := json.Marshal(s)
	var out State
	_ = json.Unmarshal(raw, &out)
	return &out
}
