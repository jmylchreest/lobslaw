package turn

import (
	"encoding/json"
	"maps"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/identity"
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
	Request  Request
	Messages []Message
}

func EncodeContinuation(c *Continuation) *lobslawv1.Continuation {
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
		Claims:              ClaimsToProto(c.Request.Claims),
		BotId:               c.Request.BotID,
		Principal:           c.Request.Principal.String(),
	}
	state := c.Request.Spent
	out.SpentUsd = state.SpendUSD
	out.ToolCalls = int32(state.ToolCalls)
	out.EgressBytes = state.EgressBytes
	for _, m := range c.Messages {
		out.Messages = append(out.Messages, MessageToProto(m))
	}

	// Older readers ignore prepared metadata, so persist the effective arguments
	// in the existing tool-call field too. New readers recover the original binding.
	for _, m := range c.Messages {
		if p := m.PreparedToolCall; p != nil {
			raw, _ := json.Marshal(p.Params) // map[string]string is always JSON encodable.
			for _, wire := range out.Messages {
				for _, tc := range wire.ToolCalls {
					if tc.Id == p.CallID && tc.Name == p.ToolName && tc.Arguments == p.OriginalArguments {
						tc.Arguments = string(raw)
					}
				}
			}
		}
	}
	return out
}

// DecodeContinuation rebuilds a paused turn.
//
// caps come from the resuming node's config rather than the record: a
// budget cap is an operator's current policy, and restoring a cap from
// a paused turn would let an old one outlive the change.
func DecodeContinuation(p *lobslawv1.Continuation, caps BudgetCaps) (*Continuation, error) {
	if p == nil {
		return nil, nil
	}
	// Replay the spend so a resumed turn does not start over with a
	// full allowance — otherwise every confirmation would be a way to
	// double the budget.
	spent := BudgetState{
		ToolCalls:   int(p.ToolCalls),
		SpendUSD:    p.SpentUsd,
		EgressBytes: p.EgressBytes,
	}

	out := &Continuation{
		Request: Request{
			Message:             p.UserMessage,
			Claims:              ClaimsFromProto(p.Claims),
			BotID:               p.BotId,
			Principal:           identity.Principal(p.Principal),
			UserTimezone:        p.UserTimezone,
			Model:               p.Model,
			SystemPrompt:        p.SystemPrompt,
			ConversationSummary: p.ConversationSummary,
			RecalledContext:     p.RecalledContext,
			Caps:                caps,
			Spent:               spent,
		},
	}
	for _, m := range p.Messages {
		out.Messages = append(out.Messages, MessageFromProto(m))
	}

	for _, m := range out.Messages {
		if p := m.PreparedToolCall; p != nil {
			raw, _ := json.Marshal(p.Params)
			for i := range out.Messages {
				for j := range out.Messages[i].ToolCalls {
					tc := &out.Messages[i].ToolCalls[j]
					if tc.ID == p.CallID && tc.Name == p.ToolName && tc.Arguments == string(raw) {
						tc.Arguments = p.OriginalArguments
					}
				}
			}
		}
	}
	return out, nil
}

func MessageToProto(m Message) *lobslawv1.SessionMessage {
	out := &lobslawv1.SessionMessage{
		Role:          m.Role,
		Content:       m.Content,
		ToolCallId:    m.ToolCallID,
		BudgetPending: m.BudgetPending,
	}
	if p := m.PreparedToolCall; p != nil {
		out.PreparedToolCall = &lobslawv1.PreparedToolCall{CallId: p.CallID, ToolName: p.ToolName, TurnId: p.TurnID, OriginalArguments: p.OriginalArguments, Params: maps.Clone(p.Params), DispatchKind: p.DispatchKind}
		for _, op := range p.Approvals {
			out.PreparedToolCall.Approvals = append(out.PreparedToolCall.Approvals, &lobslawv1.PreparedApproval{Action: op.Action, Resource: op.Resource})
		}
	}
	for _, tc := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, &lobslawv1.SessionToolCall{
			Id: tc.ID, Name: tc.Name, Arguments: tc.Arguments,
		})
	}
	return out
}

func MessageFromProto(m *lobslawv1.SessionMessage) Message {
	if m == nil {
		return Message{}
	}
	out := Message{
		Role:          m.Role,
		Content:       m.Content,
		ToolCallID:    m.ToolCallId,
		BudgetPending: m.BudgetPending,
	}
	if p := m.PreparedToolCall; p != nil {
		out.PreparedToolCall = &PreparedToolCall{CallID: p.CallId, ToolName: p.ToolName, TurnID: p.TurnId, OriginalArguments: p.OriginalArguments, Params: maps.Clone(p.Params), DispatchKind: p.DispatchKind}
		for _, op := range p.Approvals {
			out.PreparedToolCall.Approvals = append(out.PreparedToolCall.Approvals, PreparedApproval{Action: op.Action, Resource: op.Resource})
		}
	}
	for _, tc := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID: tc.Id, Name: tc.Name, Arguments: tc.Arguments,
		})
	}
	return out
}

func ClaimsToProto(c *types.Claims) *lobslawv1.Claims {
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

func ClaimsFromProto(p *lobslawv1.Claims) *types.Claims {
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
