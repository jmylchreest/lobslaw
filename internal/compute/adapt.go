package compute

import (
	"context"

	"github.com/jmylchreest/lobslaw/internal/turn"
)

// Adapt returns agent as a turn.Runner. Nil stays nil so a node
// without compute still constructs the HTTP surface.
func Adapt(agent *Agent) turn.Runner {
	if agent == nil {
		return nil
	}
	return agent
}

func (a *Agent) Run(ctx context.Context, req turn.Request) (*turn.Response, error) {
	creq, err := requestFromTurn(req)
	if err != nil {
		return nil, err
	}
	resp, err := a.RunToolCallLoop(ctx, creq)
	return responseToTurn(resp), err
}

func (a *Agent) Resume(ctx context.Context, req turn.Request, prior []turn.Message) (*turn.Response, error) {
	creq, err := requestFromTurn(req)
	if err != nil {
		return nil, err
	}
	if creq.Budget != nil {
		creq.Budget.Relax()
	}
	resp, err := a.ResumeFromConfirmation(ctx, creq, prior)
	return responseToTurn(resp), err
}

func requestFromTurn(req turn.Request) (ProcessMessageRequest, error) {
	budget, err := NewTurnBudget(req.Caps)
	if err != nil {
		return ProcessMessageRequest{}, err
	}
	budget.Restore(req.Spent)
	return ProcessMessageRequest{
		Message:             req.Message,
		Claims:              req.Claims,
		Principal:           req.Principal,
		TurnID:              req.TurnID,
		Channel:             req.Channel,
		ChannelID:           req.ChannelID,
		SharedConversation:  req.SharedConversation,
		Hint:                Hint(req.Hint),
		UserTimezone:        req.UserTimezone,
		SystemPrompt:        req.SystemPrompt,
		Model:               req.Model,
		Budget:              budget,
		ConversationHistory: req.ConversationHistory,
		ConversationSummary: req.ConversationSummary,
		RecalledContext:     req.RecalledContext,
		Attachments:         req.Attachments,
		BotID:               req.BotID,
	}, nil
}

func responseToTurn(resp *ProcessMessageResponse) *turn.Response {
	if resp == nil {
		return nil
	}
	return &turn.Response{
		Reply:                 resp.Reply,
		ToolCalls:             resp.ToolCalls,
		Attachments:           resp.Attachments,
		Messages:              resp.Messages,
		TurnStartIndex:        resp.TurnStartIndex,
		BudgetState:           resp.BudgetState,
		NeedsConfirmation:     resp.NeedsConfirmation,
		ConfirmationAction:    resp.ConfirmationAction,
		ConfirmationResource:  resp.ConfirmationResource,
		ConfirmationGrantable: resp.ConfirmationGrantable,
		ConfirmationLabels:    resp.ConfirmationLabels,
		ConfirmationReason:    resp.ConfirmationReason,
	}
}
