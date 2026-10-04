package node

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type teamTaskRunner struct{ n *Node }

func (n *Node) botTaskStarter() func(context.Context, turn.Request) (*pb.TaskApprovalRecord, error) {
	if !n.teamsActive() || n.agent == nil || n.inboxSvc == nil {
		return nil
	}
	return n.startBotChatTask
}

func (n *Node) startBotChatTask(ctx context.Context, request turn.Request) (*pb.TaskApprovalRecord, error) {
	profile, err := botResolverOrNil(n.botSvc).ResolveBot(ctx, request.BotID)
	if err != nil {
		return nil, err
	}
	leg, cancel := context.WithTimeout(ctx, inboxExecutionTimeout)
	defer cancel()
	req := compute.ProcessMessageRequest{Bot: profile, BotID: request.BotID, Claims: request.Claims, Principal: profile.Principal(), Message: request.Message, TurnID: request.TurnID}
	if profile.IsCoordinator {
		req.ConversationHistory = append(req.ConversationHistory, request.ConversationHistory...)
		history, err := n.coordinatorTaskHistory(profile.Owner, profile.ID)
		if err != nil {
			return nil, err
		}
		req.ConversationHistory = append(req.ConversationHistory, history...)
		req.ConversationSummary, req.Channel, req.ChannelID = request.ConversationSummary, request.Channel, request.ChannelID
	}
	response, err := n.startTask(leg, req, nil, profile.IsCoordinator)
	if err != nil && response == nil {
		return nil, err
	}
	task, err := n.taskApprovalAPI().GetTaskApproval(ctx, &pb.GetTaskApprovalRequest{Id: response.TaskID, Owner: profile.Owner})
	if err != nil {
		return nil, err
	}
	return task.Record, nil
}

func (r teamTaskRunner) RunToolCallLoop(ctx context.Context, req compute.ProcessMessageRequest) (*compute.ProcessMessageResponse, error) {
	return r.n.startTeamTask(ctx, req, nil)
}

// resolveTeamAuthority uses the current bot record and operator-declared roles.
// Saved JWT roles are only an upper bound, never today's source of authority.
func (n *Node) resolveTeamAuthority(ctx context.Context, owner, actor string) (compute.TaskAuthority, error) {
	if !strings.HasPrefix(actor, "bot:") || !strings.HasPrefix(owner, "user:") || n.botSvc == nil {
		return compute.TaskAuthority{}, errors.New("team task requires an owned bot")
	}
	profile, err := botResolverOrNil(n.botSvc).ResolveBot(ctx, strings.TrimPrefix(actor, "bot:"))
	if err != nil {
		return compute.TaskAuthority{}, err
	}
	if profile.Owner != owner {
		return compute.TaskAuthority{}, errors.New("task bot ownership changed")
	}
	userID := strings.TrimPrefix(owner, "user:")
	if len(n.cfg.Users) > 0 {
		found := false
		for _, user := range n.cfg.Users {
			found = found || identity.User(user.ID).String() == owner
		}
		if !found {
			return compute.TaskAuthority{}, errors.New("task owner is no longer configured")
		}
	}
	budget, err := compute.NewTurnBudget(compute.FromLimits(n.cfg.Compute.Limits))
	if err != nil {
		return compute.TaskAuthority{}, err
	}
	budget.Tighten(profile.Caps)
	return compute.TaskAuthority{Claims: &types.Claims{UserID: userID, Roles: n.resolveUserRoles(userID)}, Bot: profile, Caps: budget.Caps(), ValidateOriginal: func(claims *types.Claims) error { return n.validateTeamClaims(ctx, owner, claims) }}, nil
}

func (n *Node) validateTeamClaims(ctx context.Context, owner string, claims *types.Claims) error {
	if claims == nil || (!claims.ExpiresAt.IsZero() && time.Now().After(claims.ExpiresAt)) {
		return errors.New("original task authority is missing or expired")
	}
	if strings.HasPrefix(claims.UserID, "bot:") {
		caller, err := botResolverOrNil(n.botSvc).ResolveBot(ctx, strings.TrimPrefix(claims.UserID, "bot:"))
		if err != nil {
			return err
		}
		if caller.Owner == owner {
			return nil
		}
	} else if n.identityResolver().Resolve(claims.UserID).String() == owner {
		return nil
	}
	return errors.New("original task caller no longer belongs to this owner")
}

func (n *Node) startTeamTask(ctx context.Context, req compute.ProcessMessageRequest, item *pb.BotInboxItem) (*compute.ProcessMessageResponse, error) {
	return n.startTask(ctx, req, item, false)
}

func (n *Node) startTask(ctx context.Context, req compute.ProcessMessageRequest, item *pb.BotInboxItem, conversation bool) (*compute.ProcessMessageResponse, error) {
	if n.agent == nil || n.inboxSvc == nil || req.Bot == nil || req.Claims == nil {
		return nil, errors.New("team task runner is not wired or lacks authority")
	}
	actor, owner := req.Bot.Principal().String(), req.Bot.Owner
	live, err := n.resolveTeamAuthority(ctx, owner, actor)
	if err != nil {
		return nil, err
	}
	if err := live.ValidateOriginal(req.Claims); err != nil {
		return nil, err
	}
	claims := *req.Claims
	claims.Roles = slices.DeleteFunc(slices.Clone(claims.Roles), func(role string) bool { return !slices.Contains(live.Claims.Roles, role) })
	req.Claims = &claims
	if req.Budget == nil {
		req.Budget, err = compute.NewTurnBudget(live.Caps)
		if err != nil {
			return nil, err
		}
	}
	req.Budget.Tighten(live.Caps)
	if !conversation {
		req.Bot = req.Bot.Without("ask_bot")
	}
	req.Principal = req.Bot.Principal()
	if req.TurnID == "" {
		req.TurnID = ids.New()
	}
	backend := n.taskApprovalAPI()
	if item == nil {
		caller, _ := turn.IdentityFrom(ctx)
		item = &pb.BotInboxItem{Recipient: req.BotID, Sender: caller.Principal.String(), RequestedBy: owner, Kind: pb.InboxKind_INBOX_KIND_QUESTION, Body: req.Message}
	}
	create := &pb.CreateTaskApprovalRequest{Owner: owner, Actor: actor, ParentId: compute.TaskIDFrom(ctx), Inbox: item, InboxMaxPending: n.inboxSvc.AdmissionLimit(), CoordinatorConversation: conversation}
	// Each tool checks the durable task deadline, including during the initial
	// leg. An original token expiring sooner must shorten that authority too.
	if !claims.ExpiresAt.IsZero() && time.Until(claims.ExpiresAt) < memory.TaskApprovalTTL {
		create.ExpiresAt = timestamppb.New(claims.ExpiresAt)
	}
	created, err := backend.CreateTaskApproval(ctx, create)
	if err != nil {
		return nil, err
	}
	task := created.Record
	task.TurnId = req.TurnID
	item = created.Inbox
	if item == nil {
		return nil, errors.New("task backend did not atomically admit work")
	}
	ctx = compute.WithTaskExecution(ctx, turn.TaskScope{ID: task.Id, Owner: owner, Actor: actor, ClaimToken: task.ClaimedBy, CoordinatorConversation: task.CoordinatorConversation}, backend.CheckGrantTaskApproval)
	response, err := n.agent.RunToolCallLoop(ctx, req)
	if err != nil {
		if response == nil {
			response = &compute.ProcessMessageResponse{}
		}
		response.TaskID = task.Id
		commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), compute.TaskCommitTimeout)
		defer cancel()
		unknown, saveErr := compute.FinishTask(commitCtx, backend, task, response, err)
		if saveErr == nil {
			saveErr = n.reconcileTeamTask(commitCtx, item, unknown)
		}
		return response, errors.Join(fmt.Errorf("task %s outcome may be uncertain: %w", task.Id, err), saveErr)
	}
	response.TaskID = task.Id
	if response.NeedsConfirmation {
		_, err = compute.PauseTask(ctx, backend, task, req, response)
		return response, err
	}
	finished, err := compute.FinishTask(ctx, backend, task, response, nil)
	if err != nil {
		return nil, err
	}
	return response, n.reconcileTeamTask(ctx, item, finished)
}

// The queue is the scheduler; TaskApprovalService is the sole execution fence.
// Waiting items release their worker and are discoverable through the owner API.
func (n *Node) resumeTeamTasks(ctx context.Context, recipient string) error {
	items, err := n.inboxSvc.List(ctx, recipient, memory.InboxFilter{Statuses: []pb.InboxStatus{pb.InboxStatus_INBOX_STATUS_WAITING}})
	if err != nil {
		return err
	}
	for _, item := range items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if item.TaskId == "" || item.RequestedBy == "" {
			continue
		}
		backend := n.taskApprovalAPI()
		got, err := backend.GetTaskApproval(ctx, &pb.GetTaskApprovalRequest{Id: item.TaskId, Owner: item.RequestedBy})
		if err != nil {
			return err
		}
		task := got.Record
		if task.State == pb.TaskApprovalState_TASK_APPROVAL_STATE_READY {
			runner := compute.TaskRunner{Backend: backend, Agent: n.agent, Resolve: n.resolveTeamAuthority}
			leg, cancel := context.WithTimeout(ctx, inboxExecutionTimeout)
			task, err = runner.Resume(leg, &pb.ClaimTaskApprovalRequest{Id: task.Id, Owner: task.Owner, Actor: task.Actor, Revision: task.Revision})
			cancel()
			if err != nil {
				return err
			}
		}
		if err := n.reconcileTeamTask(ctx, item, task); err != nil {
			return err
		}
	}
	return nil
}

func (n *Node) reconcileTeamTask(ctx context.Context, item *pb.BotInboxItem, task *pb.TaskApprovalRecord) error {
	outcome := memory.InboxOutcome{ClaimRevision: item.Revision, Claimer: item.ClaimedBy, Result: task.Result, SessionID: task.SessionId, CostUSD: task.GetBudgetSpent().GetSpendUsd()}
	for _, receipt := range task.Receipts {
		if receipt.ExecutionStatus == turn.ReceiptExecuted && !slices.Contains(outcome.ToolsUsed, receipt.ToolName) {
			outcome.ToolsUsed = append(outcome.ToolsUsed, receipt.ToolName)
		}
	}
	switch task.State {
	case pb.TaskApprovalState_TASK_APPROVAL_STATE_COMPLETED:
	case pb.TaskApprovalState_TASK_APPROVAL_STATE_OUTCOME_UNKNOWN:
		if task.Recoverable {
			return nil
		}
		outcome.Err = errors.New("execution outcome uncertain; no recoverable checkpoint, no replay; owner may close the task through cancel")
	case pb.TaskApprovalState_TASK_APPROVAL_STATE_DENIED, pb.TaskApprovalState_TASK_APPROVAL_STATE_EXPIRED, pb.TaskApprovalState_TASK_APPROVAL_STATE_CANCELLED:
		outcome.Err = fmt.Errorf("task %s: %s", task.Id, task.State)
	default:
		return nil
	}
	_, err := n.inboxSvc.Resolve(ctx, item.Recipient, item.Id, outcome)
	return err
}
