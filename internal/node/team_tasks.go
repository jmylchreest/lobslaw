package node

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type teamTaskRunner struct{ n *Node }

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
	return compute.TaskAuthority{Claims: &types.Claims{UserID: userID, Roles: n.resolveUserRoles(userID)}, Bot: profile.Without("ask_bot"), Caps: budget.Caps(), ValidateOriginal: func(claims *types.Claims) error { return n.validateTeamClaims(ctx, owner, claims) }}, nil
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
	req.Bot = req.Bot.Without("ask_bot")
	req.Principal = req.Bot.Principal()
	req.TurnID = ids.New()
	backend := n.taskApprovalAPI()
	created, err := backend.CreateTaskApproval(ctx, &pb.CreateTaskApprovalRequest{Owner: owner, Actor: actor, ParentId: compute.TaskIDFrom(ctx)})
	if err != nil {
		return nil, err
	}
	task := created.Record
	if item == nil {
		caller, _ := turn.IdentityFrom(ctx)
		item = &pb.BotInboxItem{Recipient: req.BotID, Sender: caller.Principal.String(), RequestedBy: owner, Kind: pb.InboxKind_INBOX_KIND_QUESTION, Body: req.Message}
	}
	item, err = n.inboxSvc.LinkTask(ctx, item, task.Id)
	if err != nil {
		return nil, fmt.Errorf("link task before execution: %w", err)
	}
	ctx = compute.WithTaskExecution(ctx, turn.TaskScope{ID: task.Id, Owner: owner, Actor: actor, ClaimToken: task.ClaimedBy}, backend.CheckGrantTaskApproval)
	response, err := n.agent.RunToolCallLoop(ctx, req)
	if err != nil {
		// Leave the durable execution claim alone: loss of the reply does not
		// establish whether a tool already performed its external effect.
		return nil, fmt.Errorf("task %s outcome may be uncertain: %w", task.Id, err)
	}
	response.TaskID = task.Id
	if response.NeedsConfirmation {
		_, err = compute.PauseTask(ctx, backend, task, req, response)
		return response, err
	}
	finished, err := backend.FinishTaskApproval(ctx, &pb.FinishTaskApprovalRequest{Id: task.Id, Owner: owner, Actor: actor, Revision: task.Revision, ClaimToken: task.ClaimedBy, Result: response.Reply})
	if err != nil {
		return nil, err
	}
	return response, n.reconcileTeamTask(ctx, item, finished.Record)
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
	outcome := memory.InboxOutcome{ClaimRevision: item.Revision, Claimer: item.ClaimedBy, Result: task.Result}
	switch task.State {
	case pb.TaskApprovalState_TASK_APPROVAL_STATE_COMPLETED:
	case pb.TaskApprovalState_TASK_APPROVAL_STATE_DENIED, pb.TaskApprovalState_TASK_APPROVAL_STATE_EXPIRED, pb.TaskApprovalState_TASK_APPROVAL_STATE_CANCELLED:
		outcome.Err = fmt.Errorf("task %s: %s", task.Id, task.State)
	default:
		return nil
	}
	_, err := n.inboxSvc.Resolve(ctx, item.Recipient, item.Id, outcome)
	return err
}
