package compute

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/commandrisk"
	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func requestToProto(req turn.Request) *lobslawv1.RunTurnRequest {
	out := &lobslawv1.RunTurnRequest{
		Message:             req.Message,
		Claims:              claimsToProto(req.Claims),
		Principal:           req.Principal.String(),
		TurnId:              req.TurnID,
		Channel:             req.Channel,
		ChannelId:           req.ChannelID,
		SharedConversation:  req.SharedConversation,
		Hint:                req.Hint,
		UserTimezone:        req.UserTimezone,
		SystemPrompt:        req.SystemPrompt,
		Model:               req.Model,
		ConversationSummary: req.ConversationSummary,
		RecalledContext:     req.RecalledContext,
		BotId:               req.BotID,
		Caps: &lobslawv1.TurnBudgetCaps{
			MaxToolCalls:   int32(req.Caps.MaxToolCalls),
			MaxSpendUsd:    req.Caps.MaxSpendUSD,
			MaxEgressBytes: req.Caps.MaxEgressBytes,
		},
		Spent: &lobslawv1.TurnBudgetState{
			ToolCalls:   int32(req.Spent.ToolCalls),
			SpendUsd:    req.Spent.SpendUSD,
			EgressBytes: req.Spent.EgressBytes,
		},
	}
	if out.Principal == "" && req.Claims != nil {
		out.Principal = identity.User(req.Claims.UserID).String()
	}
	for _, m := range req.ConversationHistory {
		out.ConversationHistory = append(out.ConversationHistory, messageToProto(m))
	}
	for _, a := range req.Attachments {
		out.Attachments = append(out.Attachments, attachmentToProto(a))
	}
	return out
}

func requestFromProto(p *lobslawv1.RunTurnRequest) turn.Request {
	if p == nil {
		return turn.Request{}
	}
	out := turn.Request{
		Message:             p.Message,
		Claims:              claimsFromProto(p.Claims),
		Principal:           identity.Principal(p.Principal),
		TurnID:              p.TurnId,
		Channel:             p.Channel,
		ChannelID:           p.ChannelId,
		SharedConversation:  p.SharedConversation,
		Hint:                p.Hint,
		UserTimezone:        p.UserTimezone,
		SystemPrompt:        p.SystemPrompt,
		Model:               p.Model,
		ConversationSummary: p.ConversationSummary,
		RecalledContext:     p.RecalledContext,
		BotID:               p.BotId,
	}
	if p.Caps != nil {
		out.Caps = turn.BudgetCaps{
			MaxToolCalls:   int(p.Caps.MaxToolCalls),
			MaxSpendUSD:    p.Caps.MaxSpendUsd,
			MaxEgressBytes: p.Caps.MaxEgressBytes,
		}
	}
	if p.Spent != nil {
		out.Spent = turn.BudgetState{
			ToolCalls:   int(p.Spent.ToolCalls),
			SpendUSD:    p.Spent.SpendUsd,
			EgressBytes: p.Spent.EgressBytes,
		}
	}
	for _, m := range p.ConversationHistory {
		out.ConversationHistory = append(out.ConversationHistory, messageFromProto(m))
	}
	for _, a := range p.Attachments {
		out.Attachments = append(out.Attachments, attachmentFromProto(a))
	}
	return out
}

func responseToProto(resp *turn.Response) *lobslawv1.RunTurnResponse {
	if resp == nil {
		return nil
	}
	out := &lobslawv1.RunTurnResponse{
		Reply:                 resp.Reply,
		TurnStartIndex:        int32(resp.TurnStartIndex),
		NeedsConfirmation:     resp.NeedsConfirmation,
		ConfirmationAction:    resp.ConfirmationAction,
		ConfirmationResource:  resp.ConfirmationResource,
		ConfirmationGrantable: resp.ConfirmationGrantable,
		ConfirmationReason:    resp.ConfirmationReason,
		Budget: &lobslawv1.TurnBudgetState{
			ToolCalls:   int32(resp.BudgetState.ToolCalls),
			SpendUsd:    resp.BudgetState.SpendUSD,
			EgressBytes: resp.BudgetState.EgressBytes,
		},
	}
	for _, tc := range resp.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, &lobslawv1.TurnToolInvocation{
			CallId:   tc.CallID,
			ToolName: tc.ToolName,
			Args:     tc.Args,
			Output:   tc.Output,
			ExitCode: int32(tc.ExitCode),
			Error:    tc.Error,
		})
	}
	for _, a := range resp.Attachments {
		out.Attachments = append(out.Attachments, attachmentToProto(a))
	}
	for _, m := range resp.Messages {
		out.Messages = append(out.Messages, messageToProto(m))
	}
	for _, l := range resp.ConfirmationLabels {
		out.ConfirmationLabels = append(out.ConfirmationLabels, string(l))
	}
	return out
}

func responseFromProto(p *lobslawv1.RunTurnResponse) *turn.Response {
	if p == nil {
		return nil
	}
	out := &turn.Response{
		Reply:                 p.Reply,
		TurnStartIndex:        int(p.TurnStartIndex),
		NeedsConfirmation:     p.NeedsConfirmation,
		ConfirmationAction:    p.ConfirmationAction,
		ConfirmationResource:  p.ConfirmationResource,
		ConfirmationGrantable: p.ConfirmationGrantable,
		ConfirmationReason:    p.ConfirmationReason,
	}
	if p.Budget != nil {
		out.BudgetState = turn.BudgetState{
			ToolCalls:   int(p.Budget.ToolCalls),
			SpendUSD:    p.Budget.SpendUsd,
			EgressBytes: p.Budget.EgressBytes,
		}
	}
	for _, tc := range p.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, turn.ToolInvocation{
			CallID:   tc.CallId,
			ToolName: tc.ToolName,
			Args:     tc.Args,
			Output:   tc.Output,
			ExitCode: int(tc.ExitCode),
			Error:    tc.Error,
		})
	}
	for _, a := range p.Attachments {
		out.Attachments = append(out.Attachments, attachmentFromProto(a))
	}
	for _, m := range p.Messages {
		out.Messages = append(out.Messages, messageFromProto(m))
	}
	for _, l := range p.ConfirmationLabels {
		out.ConfirmationLabels = append(out.ConfirmationLabels, commandrisk.RiskLabel(l))
	}
	return out
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
	if m == nil {
		return turn.Message{}
	}
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

func attachmentToProto(a types.Attachment) *lobslawv1.TurnAttachment {
	return &lobslawv1.TurnAttachment{
		Kind:      string(a.Kind),
		MimeType:  a.MimeType,
		Size:      int32(a.Size),
		Width:     int32(a.Width),
		Height:    int32(a.Height),
		Duration:  int32(a.Duration),
		Reference: a.Reference,
		Filename:  a.Filename,
		LocalPath: a.LocalPath,
	}
}

func attachmentFromProto(a *lobslawv1.TurnAttachment) types.Attachment {
	if a == nil {
		return types.Attachment{}
	}
	return types.Attachment{
		Kind:      types.AttachmentKind(a.Kind),
		MimeType:  a.MimeType,
		Size:      int(a.Size),
		Width:     int(a.Width),
		Height:    int(a.Height),
		Duration:  int(a.Duration),
		Reference: a.Reference,
		Filename:  a.Filename,
		LocalPath: a.LocalPath,
	}
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
