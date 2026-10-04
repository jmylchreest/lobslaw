package console

import (
	"strings"

	"github.com/jmylchreest/lobslaw/internal/bots"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type BotView struct {
	ID            string   `json:"id"`
	DisplayName   string   `json:"display_name"`
	Description   string   `json:"description"`
	Instructions  string   `json:"instructions"`
	IsCoordinator bool     `json:"is_coordinator"`
	GroupID       string   `json:"group_id"`
	Enabled       bool     `json:"enabled"`
	Tools         []string `json:"tools"`
	MayMessage    []string `json:"may_message"`
	Revision      uint64   `json:"revision"`
	CreatedAt     string   `json:"created_at,omitempty"`
	UpdatedAt     string   `json:"updated_at,omitempty"`
}

type InboxItemView struct {
	Revision        uint64   `json:"revision"`
	TruncatedFields []string `json:"truncated_fields,omitempty"`
	DetailPath      string   `json:"detail_path,omitempty"`
	TaskID          string   `json:"task_id,omitempty"`
	ID              string   `json:"id"`
	Recipient       string   `json:"recipient"`
	Sender          string   `json:"sender"`
	Kind            string   `json:"kind"`
	Subject         string   `json:"subject"`
	Body            string   `json:"body,omitempty"`
	Priority        int32    `json:"priority"`
	Status          string   `json:"status"`
	Result          string   `json:"result,omitempty"`
	Error           string   `json:"error,omitempty"`
	Attempts        int32    `json:"attempts"`
	CorrelationID   string   `json:"correlation_id,omitempty"`
	SessionID       string   `json:"session_id,omitempty"`
	// Who asked for this, as against who put it in the queue.
	RequestedBy string `json:"requested_by,omitempty"`
	// What the turn actually did, as against what its result claims.
	ToolsUsed   []string `json:"tools_used,omitempty"`
	TokensUsed  uint64   `json:"tokens_used,omitempty"`
	CostUSD     float64  `json:"cost_usd,omitempty"`
	CreatedAt   string   `json:"created_at,omitempty"`
	CompletedAt string   `json:"completed_at,omitempty"`
}

func BotViewOf(rec *lobslawv1.BotRecord) BotView {
	out := BotView{
		ID:            rec.GetId(),
		DisplayName:   rec.GetDisplayName(),
		Description:   rec.GetDescription(),
		Instructions:  rec.GetInstructions(),
		IsCoordinator: rec.GetIsCoordinator(),
		// Raw, not resolved to a default: bots.GroupOf is explicit
		// that empty stays empty, because routing a bot into somebody
		// else's team is the failure this exists to prevent. New bots
		// always carry their owner's team id (see ensureOwnersTeam).
		GroupID:    GroupOfBot(rec),
		Enabled:    rec.GetEnabled(),
		Tools:      rec.GetTools(),
		MayMessage: rec.GetMayMessage(),
		Revision:   rec.GetRevision(),
	}
	if ts := rec.GetCreatedAt(); ts != nil {
		out.CreatedAt = ts.AsTime().UTC().Format(rfc3339)
	}
	if ts := rec.GetUpdatedAt(); ts != nil {
		out.UpdatedAt = ts.AsTime().UTC().Format(rfc3339)
	}
	// Non-nil slices so a JSON consumer gets [] rather than null and
	// does not have to special-case "this bot has no edges".
	if out.Tools == nil {
		out.Tools = []string{}
	}
	if out.MayMessage == nil {
		out.MayMessage = []string{}
	}
	return out
}

func InboxViewOf(item *lobslawv1.BotInboxItem, withBody bool) InboxItemView {
	out := InboxItemView{
		Revision:      item.GetRevision(),
		TaskID:        item.GetTaskId(),
		ID:            item.GetId(),
		Recipient:     item.GetRecipient(),
		Sender:        item.GetSender(),
		Kind:          bots.InboxKindName(item.GetKind()),
		Subject:       item.GetSubject(),
		Priority:      item.GetPriority(),
		Status:        bots.InboxStatusName(item.GetStatus()),
		Result:        item.GetResult(),
		Error:         item.GetError(),
		Attempts:      item.GetAttempts(),
		CorrelationID: item.GetCorrelationId(),
		SessionID:     item.GetSessionId(),
		RequestedBy:   item.GetRequestedBy(),
		ToolsUsed:     item.GetToolsUsed(),
		TokensUsed:    item.GetTokensUsed(),
		CostUSD:       item.GetCostUsd(),
	}
	if withBody {
		out.Body = item.GetBody()
	}
	if ts := item.GetCreatedAt(); ts != nil {
		out.CreatedAt = ts.AsTime().UTC().Format(rfc3339)
	}
	if ts := item.GetCompletedAt(); ts != nil {
		out.CompletedAt = ts.AsTime().UTC().Format(rfc3339)
	}
	return out
}

const rfc3339 = "2006-01-02T15:04:05Z07:00"

type GroupView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Coordinator string `json:"coordinator_bot_id,omitempty"`
	IsDefault   bool   `json:"is_default"`
	Owner       string `json:"owner,omitempty"`
	// Mine says whether the person asking may change this team, so the
	// console can hide controls that would only fail.
	Mine     bool   `json:"mine"`
	Revision uint64 `json:"revision"`
	// Bots is how many belong to this team, so a switcher can say
	// "Engineering · 4" without a second request per group.
	Bots int `json:"bots"`
}

func GroupViewOf(rec *lobslawv1.GroupRecord, count int, principal string) GroupView {
	return GroupView{
		Owner:       rec.GetOwner(),
		Mine:        bots.MayModifyGroup(rec, principal),
		ID:          rec.GetId(),
		Name:        rec.GetName(),
		Description: rec.GetDescription(),
		Coordinator: rec.GetCoordinatorBotId(),
		IsDefault:   rec.GetIsDefault(),
		Revision:    rec.GetRevision(),
		Bots:        count,
	}
}

func GroupOfBot(rec *lobslawv1.BotRecord) string { return strings.TrimSpace(rec.GetGroupId()) }
