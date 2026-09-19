package gateway

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// A confirmation is a turn stopped mid-flight. Resuming it needs the
// transcript so far and the budget already spent — both of which used
// to live in a Go map on one handler, which is exactly why an approval
// after a restart could only tell the user to send it again.
//
// Everything here is conversation state. Tools are not: they are
// rebuilt from the resuming node's own registry, because a serialised
// tool definition would outlive a redeploy that changed it.

// Continuation is the serialisable half of a paused turn.
type Continuation struct {
	Request  turn.Request
	Messages []turn.Message
}

func continuationRequest(req turn.Request, resp *turn.Response) turn.Request {
	if resp != nil {
		req.Spent = resp.BudgetState
	}
	return req
}

func continuationToProto(c *Continuation) *lobslawv1.Continuation {
	if c == nil {
		return nil
	}
	out := &lobslawv1.Continuation{
		UserMessage:         c.Request.Message,
		UserTimezone:        c.Request.UserTimezone,
		Model:               c.Request.Model,
		SystemPrompt:        c.Request.SystemPrompt,
		ConversationSummary: c.Request.ConversationSummary,
		RecalledContext:     c.Request.RecalledContext,
		Claims:              claimsToProto(c.Request.Claims),
		BotId:               c.Request.BotID,
		Principal:           c.Request.Principal.String(),
	}
	out.SpentUsd = c.Request.Spent.SpendUSD
	out.ToolCalls = int32(c.Request.Spent.ToolCalls)
	out.EgressBytes = c.Request.Spent.EgressBytes
	for _, m := range c.Messages {
		out.Messages = append(out.Messages, messageToProto(m))
	}
	return out
}

// continuationFromProto rebuilds a paused turn.
//
// caps come from the resuming node's config rather than the record: a
// budget cap is an operator's current policy, and restoring a cap from
// a paused turn would let an old one outlive the change.
func continuationFromProto(p *lobslawv1.Continuation, caps turn.BudgetCaps) (*Continuation, error) {
	if p == nil {
		return nil, nil
	}
	// Replay the spend so a resumed turn does not start over with a
	// full allowance — otherwise every confirmation would be a way to
	// double the budget. Caps come from the resuming node's config.
	out := &Continuation{
		Request: turn.Request{
			Message:             p.UserMessage,
			Claims:              claimsFromProto(p.Claims),
			BotID:               p.BotId,
			Principal:           identity.Principal(p.Principal),
			UserTimezone:        p.UserTimezone,
			Model:               p.Model,
			SystemPrompt:        p.SystemPrompt,
			ConversationSummary: p.ConversationSummary,
			RecalledContext:     p.RecalledContext,
			Caps:                caps,
			Spent: turn.BudgetState{
				ToolCalls:   int(p.ToolCalls),
				SpendUSD:    p.SpentUsd,
				EgressBytes: p.EgressBytes,
			},
		},
	}
	for _, m := range p.Messages {
		out.Messages = append(out.Messages, messageFromProto(m))
	}
	return out, nil
}

func messageToProto(m turn.Message) *lobslawv1.SessionMessage {
	out := &lobslawv1.SessionMessage{
		Role:       m.Role,
		Content:    m.Content,
		ToolCallId: m.ToolCallID,
	}
	for _, tc := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, &lobslawv1.SessionToolCall{
			Id: tc.ID, Name: tc.Name, Arguments: tc.Arguments,
		})
	}
	return out
}

func messageFromProto(m *lobslawv1.SessionMessage) turn.Message {
	out := turn.Message{
		Role:       m.Role,
		Content:    m.Content,
		ToolCallID: m.ToolCallId,
	}
	for _, tc := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, turn.ToolCall{
			ID: tc.Id, Name: tc.Name, Arguments: tc.Arguments,
		})
	}
	return out
}

func claimsToProto(c *types.Claims) *lobslawv1.Claims {
	if c == nil {
		return nil
	}
	out := &lobslawv1.Claims{
		UserId:   c.UserID,
		Issuer:   c.Issuer,
		Audience: c.Audience,
		Scope:    c.Scope,
	}
	if len(c.Roles) > 0 {
		out.Roles = append([]string(nil), c.Roles...)
	}
	if !c.ExpiresAt.IsZero() {
		out.ExpiresAt = timestamppb.New(c.ExpiresAt)
	}
	if !c.IssuedAt.IsZero() {
		out.IssuedAt = timestamppb.New(c.IssuedAt)
	}
	return out
}

func claimsFromProto(p *lobslawv1.Claims) *types.Claims {
	if p == nil {
		return nil
	}
	out := &types.Claims{
		UserID:   p.UserId,
		Issuer:   p.Issuer,
		Audience: p.Audience,
		Scope:    p.Scope,
	}
	if len(p.Roles) > 0 {
		out.Roles = append([]string(nil), p.Roles...)
	}
	if p.ExpiresAt != nil {
		out.ExpiresAt = p.ExpiresAt.AsTime()
	}
	if p.IssuedAt != nil {
		out.IssuedAt = p.IssuedAt.AsTime()
	}
	return out
}
