package compute

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// TaskApprovalBackend is shared by local and typed-gRPC adapters. The backend
// must fail closed when durable authority cannot be established.
type TaskApprovalBackend interface {
	GetTaskApproval(context.Context, *pb.GetTaskApprovalRequest) (*pb.GetTaskApprovalResponse, error)
	PauseTaskApproval(context.Context, *pb.PauseTaskApprovalRequest) (*pb.PauseTaskApprovalResponse, error)
	ClaimTaskApproval(context.Context, *pb.ClaimTaskApprovalRequest) (*pb.ClaimTaskApprovalResponse, error)
	FinishTaskApproval(context.Context, *pb.FinishTaskApprovalRequest) (*pb.FinishTaskApprovalResponse, error)
	CheckGrantTaskApproval(context.Context, *pb.CheckGrantTaskApprovalRequest) (*pb.CheckGrantTaskApprovalResponse, error)
}
type TaskAgent interface {
	ResumeFromConfirmation(context.Context, ProcessMessageRequest, []Message) (*ProcessMessageResponse, error)
	TurnIdentityFor(ProcessMessageRequest) turn.Identity
}

// TaskAuthority is reconstructed from current configuration, never the saved
// claims. Tools are rebuilt on the executing node; existing caps are rechecked.
type TaskAuthority struct {
	Claims *types.Claims
	Tools  []Tool
	Caps   BudgetCaps
	Bot    *BotProfile
	// ValidateOriginal resolves the original caller, including bot callers,
	// against current ownership without replacing its claims with the owner's.
	ValidateOriginal func(*types.Claims) error
}
type TaskRunner struct {
	Backend TaskApprovalBackend
	Agent   TaskAgent
	Resolve func(context.Context, string, string) (TaskAuthority, error)
}

// PauseTask saves the pending invocation before reporting that work is waiting.
// It accepts the runner's response, never a replacement command from an approval
// callback. The returned metadata intentionally omits the continuation.
func PauseTask(ctx context.Context, backend TaskApprovalBackend, task *pb.TaskApprovalRecord, req ProcessMessageRequest, resp *ProcessMessageResponse) (*pb.TaskApprovalRecord, error) {
	if req.Budget == nil {
		return nil, errors.New("task budget required")
	}
	return pauseTaskWithPolicy(ctx, backend, task, req, resp, req.Budget.Caps())
}

func pauseTaskWithPolicy(ctx context.Context, backend TaskApprovalBackend, task *pb.TaskApprovalRecord, req ProcessMessageRequest, resp *ProcessMessageResponse, policy BudgetCaps) (*pb.TaskApprovalRecord, error) {
	if task == nil || resp == nil || !resp.NeedsConfirmation || backend == nil {
		return nil, errors.New("task pause requires a pending response and durable backend")
	}
	op := &pb.TaskOperation{Summary: resp.ConfirmationReason}
	if resp.ConfirmationAction == "" {
		op.RequiresBudgetExtension = true
	} else {
		tc, idx, ok := pendingToolCall(resp.Messages)
		if !ok || resp.Messages[idx].PreparedToolCall == nil {
			return nil, errors.New("task confirmation has no prepared invocation")
		}
		op.CallId = tc.ID
		op.ToolName = tc.Name
		op.Action = resp.ConfirmationAction
		op.Resource = resp.ConfirmationResource
		op.Grantable = resp.ConfirmationGrantable
		for _, l := range resp.ConfirmationLabels {
			op.Labels = append(op.Labels, string(l))
		}
	}
	wire := EncodeContinuation(&TaskContinuation{Request: req, Messages: resp.Messages})
	// The response's counters are authoritative, including remote runners whose
	// request budget object did not see the execution.
	wire.SpentUsd = resp.BudgetState.SpendUSD
	wire.ToolCalls = int32(resp.BudgetState.ToolCalls)
	wire.EgressBytes = resp.BudgetState.EgressBytes
	out, err := backend.PauseTaskApproval(ctx, &pb.PauseTaskApprovalRequest{Id: task.Id, Owner: task.Owner, Actor: task.Actor, Revision: task.Revision, ClaimToken: task.ClaimedBy, TurnId: req.TurnID, Operation: op, Continuation: wire, BudgetPolicy: taskBudgetProto(policy), BudgetLimits: taskBudgetProto(req.Budget.Caps()), Receipts: taskReceipts(resp.ToolCalls), TranscriptStart: taskTranscriptStart(task, resp)})
	if err != nil {
		return nil, err
	}
	return out.Record, nil
}

// Resume claims a saved checkpoint exactly once and dispatches under live
// authority. Any error after the claim is deliberately not retried: the caller
// cannot generally know whether an external effect already happened.
func (r *TaskRunner) Resume(ctx context.Context, q *pb.ClaimTaskApprovalRequest) (*pb.TaskApprovalRecord, error) {
	if r.Backend == nil || r.Agent == nil || r.Resolve == nil || q == nil {
		return nil, errors.New("task runner requires backend, agent and current authority resolver")
	}
	authority, err := r.Resolve(ctx, q.Owner, q.Actor)
	if err != nil {
		return nil, err
	}
	if authority.Claims == nil {
		return nil, errors.New("task owner no longer has execution authority")
	}
	principal := identity.Principal("")
	if authority.Bot != nil {
		principal = authority.Bot.Principal()
	}
	if id := r.Agent.TurnIdentityFor(ProcessMessageRequest{Claims: authority.Claims, Principal: principal}); id.Principal.String() != q.Actor {
		return nil, errors.New("resolved task actor does not match saved actor")
	}
	claimed, err := r.Backend.ClaimTaskApproval(ctx, q)
	if err != nil {
		return nil, err
	}
	task := claimed.Record
	if task == nil || task.Operation == nil || task.Continuation == nil {
		return nil, errors.New("claimed task has no checkpoint")
	}
	cont, err := DecodeContinuation(task.Continuation, effectiveTaskBudget(authority.Caps, task))
	if err != nil {
		return nil, err
	}
	req := cont.Request
	if authority.ValidateOriginal != nil {
		if err := authority.ValidateOriginal(req.Claims); err != nil {
			return nil, err
		}
	}
	if err := validateResumedTaskBot(req, authority.Bot, task); err != nil {
		return nil, err
	}
	// Keep the original subject and scope, and never gain a role merely because
	// the owner acquired it while this task was waiting. Live policy still runs.
	if req.Claims == nil || (!req.Claims.ExpiresAt.IsZero() && time.Now().After(req.Claims.ExpiresAt)) {
		return nil, errors.New("original task authority is missing or expired")
	}
	claims := *req.Claims
	claims.Roles = nil
	for _, role := range req.Claims.Roles {
		if slices.Contains(authority.Claims.Roles, role) {
			claims.Roles = append(claims.Roles, role)
		}
	}
	req.Claims = &claims
	req.Tools = authority.Tools
	req.Bot = resumedTaskBot(authority.Bot, task)
	req.TurnID = task.TurnId
	// Channel identity is deliberately absent: task grants never borrow the chat
	// where a user happened to click approve.
	scope := turn.TaskScope{ID: task.Id, Owner: task.Owner, Actor: task.Actor, ClaimToken: task.ClaimedBy, CoordinatorConversation: task.CoordinatorConversation}
	ctx = WithTaskExecution(ctx, scope, r.Backend.CheckGrantTaskApproval)
	ctx = context.WithValue(ctx, approvedTaskBudgetKey{}, true)
	if task.Operation.RequiresBudgetExtension {
		if decision := req.Budget.Check(); decision.Exceeded {
			response := &ProcessMessageResponse{NeedsConfirmation: true, ConfirmationReason: "current budget policy requires a fresh extension", Messages: cont.Messages, BudgetState: req.Budget.State()}
			return pauseTaskWithPolicy(ctx, r.Backend, task, req, response, authority.Caps)
		}
	} else {
		ctx = WithTurnApproval(ctx, task.Operation.Action, task.Operation.Resource)
	}
	response, err := r.Agent.ResumeFromConfirmation(ctx, req, cont.Messages)
	if err != nil {
		if response == nil {
			response = &ProcessMessageResponse{}
		}
		return FinishTask(ctx, r.Backend, task, response, err)
	}
	if response == nil {
		return nil, errors.New("task execution returned no result")
	}
	if response.NeedsConfirmation {
		return pauseTaskWithPolicy(ctx, r.Backend, task, req, response, authority.Caps)
	}
	out, err := FinishTask(ctx, r.Backend, task, response, nil)
	if err != nil {
		return nil, fmt.Errorf("task result could not be committed; do not automatically replay: %w", err)
	}
	return out, nil
}

func restrictTaskBot(current *BotProfile, saved *pb.Continuation) *BotProfile {
	if current == nil {
		return nil
	}
	profile := *current
	profile.Denied = append(append([]string(nil), profile.Denied...), saved.BotDenied...)
	if len(saved.BotTools) > 0 {
		if len(profile.Tools) == 0 {
			profile.Tools = append([]string(nil), saved.BotTools...)
		} else {
			for _, name := range profile.Tools {
				if !slices.Contains(saved.BotTools, name) {
					profile.Denied = append(profile.Denied, name)
				}
			}
		}
	}
	return &profile
}

func resumedTaskBot(current *BotProfile, task *pb.TaskApprovalRecord) *BotProfile {
	profile := restrictTaskBot(current, task.Continuation)
	if !task.CoordinatorConversation {
		profile = profile.Without("ask_bot")
	}
	return profile
}

func validateResumedTaskBot(req ProcessMessageRequest, current *BotProfile, task *pb.TaskApprovalRecord) error {
	if current != nil && req.BotID != current.ID {
		return errors.New("saved task bot changed")
	}
	if task.CoordinatorConversation && (current == nil || !current.IsCoordinator) {
		return errors.New("coordinator classification changed; refusing saved owner context")
	}
	return nil
}
