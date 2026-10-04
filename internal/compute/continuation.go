package compute

import (
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type TaskContinuation struct {
	Request  ProcessMessageRequest
	Messages []Message
}

func EncodeContinuation(c *TaskContinuation) *lobslawv1.Continuation {
	if c == nil {
		return nil
	}
	req := turn.Request{
		Message: c.Request.Message, Claims: c.Request.Claims,
		BotID: c.Request.BotID, Principal: c.Request.Principal,
		UserTimezone: c.Request.UserTimezone, Model: c.Request.Model,
		SystemPrompt: c.Request.SystemPrompt, ConversationSummary: c.Request.ConversationSummary,
		RecalledContext: c.Request.RecalledContext,
	}
	if c.Request.Budget != nil {
		req.Spent = c.Request.Budget.State()
	}
	wire := turn.EncodeContinuation(&turn.Continuation{Request: req, Messages: c.Messages})
	if c.Request.Bot != nil {
		wire.BotTools = append([]string(nil), c.Request.Bot.Tools...)
		wire.BotDenied = append([]string(nil), c.Request.Bot.Denied...)
	}
	return wire
}

func DecodeContinuation(p *lobslawv1.Continuation, caps BudgetCaps) (*TaskContinuation, error) {
	c, err := turn.DecodeContinuation(p, caps)
	if err != nil || c == nil {
		return nil, err
	}
	req, err := requestFromTurn(c.Request)
	if err != nil {
		return nil, err
	}
	return &TaskContinuation{Request: req, Messages: c.Messages}, nil
}
