package console

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/bots"
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type InboxInput struct {
	Subject  string `json:"subject"`
	Body     string `json:"body"`
	Kind     string `json:"kind"`
	Priority int32  `json:"priority"`
}

func (s *Service) Inbox(ctx context.Context, claims *types.Claims, botID, status string, limit int) ([]InboxItemView, error) {
	if _, err := s.ownedBot(ctx, claims, botID); err != nil {
		return nil, err
	}
	if s.cfg.Inbox == nil {
		return nil, ErrUnavailable
	}
	filter := bots.InboxFilter{Limit: limit}
	if filter.Limit <= 0 {
		filter.Limit = 100
	}
	if filter.Limit > bots.MaxInboxRecentItems {
		return nil, fmt.Errorf("%w: inbox limit exceeds %d", ErrInvalid, bots.MaxInboxRecentItems)
	}
	if status != "" && status != "all" {
		v, ok := parseRESTInboxStatus(status)
		if !ok {
			return nil, fmt.Errorf("%w: unknown status %s", ErrInvalid, strconv.Quote(status))
		}
		filter.Statuses = []lobslawv1.InboxStatus{v}
	}
	items, err := s.cfg.Inbox.List(ctx, botID, filter)
	if err != nil {
		return nil, err
	}
	out := []InboxItemView{}
	for _, r := range items {
		out = append(out, InboxViewOf(r, false))
	}
	return out, nil
}
func (s *Service) PostInbox(ctx context.Context, claims *types.Claims, botID string, body InboxInput) (InboxItemView, error) {
	if _, err := s.ownedBot(ctx, claims, botID); err != nil {
		return InboxItemView{}, err
	}
	if s.cfg.Inbox == nil {
		return InboxItemView{}, ErrUnavailable
	}
	kind, ok := parseRESTInboxKind(body.Kind)
	if !ok {
		return InboxItemView{}, fmt.Errorf("%w: unknown kind %s", ErrInvalid, strconv.Quote(body.Kind))
	}
	item, err := s.cfg.Inbox.Post(ctx, &lobslawv1.BotInboxItem{Recipient: botID, Sender: "operator", RequestedBy: Principal(claims.UserID), TaskClaims: turn.ClaimsToProto(claims), Kind: kind, Subject: body.Subject, Body: body.Body, Priority: body.Priority})
	if err != nil {
		return InboxItemView{}, err
	}
	return InboxViewOf(item, true), nil
}
func (s *Service) InboxItem(ctx context.Context, claims *types.Claims, botID, id string) (InboxItemView, error) {
	if _, err := s.ownedBot(ctx, claims, botID); err != nil {
		return InboxItemView{}, err
	}
	if s.cfg.Inbox == nil {
		return InboxItemView{}, ErrUnavailable
	}
	item, err := s.cfg.Inbox.Get(ctx, botID, id)
	if err != nil {
		return InboxItemView{}, err
	}
	return InboxViewOf(item, true), nil
}
func (s *Service) ChangeInbox(ctx context.Context, claims *types.Claims, botID, id, action string) (InboxItemView, error) {
	if _, err := s.ownedBot(ctx, claims, botID); err != nil {
		return InboxItemView{}, err
	}
	if s.cfg.Inbox == nil {
		return InboxItemView{}, ErrUnavailable
	}
	var item *lobslawv1.BotInboxItem
	var err error
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "retry":
		item, err = s.cfg.Inbox.Retry(ctx, botID, id)
	case "cancel":
		item, err = s.cfg.Inbox.Cancel(ctx, botID, id)
	default:
		return InboxItemView{}, fmt.Errorf("%w: action must be retry or cancel", ErrInvalid)
	}
	if err != nil {
		return InboxItemView{}, err
	}
	return InboxViewOf(item, true), nil
}
func (s *Service) Activity(ctx context.Context, claims *types.Claims, limit int) ([]InboxItemView, error) {
	owner, err := caller(ctx, claims)
	if err != nil {
		return nil, err
	}
	if s.cfg.Bots == nil || s.cfg.Inbox == nil {
		return nil, ErrUnavailable
	}
	if limit < 0 || limit > bots.MaxInboxRecentItems {
		return nil, fmt.Errorf("%w: invalid activity limit", ErrInvalid)
	}
	if limit == 0 {
		limit = 100
	}
	rows, err := s.cfg.Bots.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]InboxItemView, 0, 2*limit)
	for _, b := range rows {
		if !bots.MayModify(b, owner) {
			continue
		}
		items, err := s.cfg.Inbox.Recent(ctx, b.Id, limit)
		if err != nil {
			return nil, err
		}
		out = collectActivity(out, items, limit)
	}
	sortInboxDescending(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func sortInboxDescending(items []InboxItemView) {
	slices.SortFunc(items, func(a, b InboxItemView) int { return cmp.Compare(b.ID, a.ID) })
}

// Keep only the newest candidates between bounded batches, including when an
// InboxAPI implementation returns more than the requested per-bot limit.
func collectActivity(out []InboxItemView, items []*lobslawv1.ConsoleInboxItem, limit int) []InboxItemView {
	for len(items) > 0 {
		n := min(limit, len(items))
		for _, item := range items[:n] {
			out = append(out, inboxSummaryToJSON(item))
		}
		sortInboxDescending(out)
		if len(out) > limit {
			clear(out[limit:])
			out = out[:limit]
		}
		items = items[n:]
	}
	return out
}

func inboxSummaryToJSON(item *lobslawv1.ConsoleInboxItem) InboxItemView {
	return InboxItemView{
		ID: item.GetId(), Recipient: item.GetRecipient(), Sender: item.GetSender(),
		Kind: item.GetKind(), Subject: item.GetSubject(), Priority: item.GetPriority(),
		Status: item.GetStatus(), Result: item.GetResult(), Error: item.GetError(),
		Attempts: item.GetAttempts(), CorrelationID: item.GetCorrelationId(),
		SessionID: item.GetSessionId(), TaskID: item.GetTaskId(), RequestedBy: item.GetRequestedBy(),
		ToolsUsed: item.GetToolsUsed(), TokensUsed: item.GetTokensUsed(), CostUSD: item.GetCostUsd(),
		CreatedAt: item.GetCreatedAt(), CompletedAt: item.GetCompletedAt(),
		Revision: item.GetRevision(), TruncatedFields: item.GetTruncatedFields(), DetailPath: item.GetDetailPath(),
	}
}

func parseRESTInboxStatus(s string) (lobslawv1.InboxStatus, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "pending":
		return lobslawv1.InboxStatus_INBOX_STATUS_PENDING, true
	case "waiting":
		return lobslawv1.InboxStatus_INBOX_STATUS_WAITING, true
	case "claimed":
		return lobslawv1.InboxStatus_INBOX_STATUS_CLAIMED, true
	case "done":
		return lobslawv1.InboxStatus_INBOX_STATUS_DONE, true
	case "failed":
		return lobslawv1.InboxStatus_INBOX_STATUS_FAILED, true
	case "cancelled", "canceled":
		return lobslawv1.InboxStatus_INBOX_STATUS_CANCELLED, true
	default:
		return lobslawv1.InboxStatus_INBOX_STATUS_UNSPECIFIED, false
	}
}

func parseRESTInboxKind(s string) (lobslawv1.InboxKind, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "task":
		return lobslawv1.InboxKind_INBOX_KIND_TASK, true
	case "question":
		return lobslawv1.InboxKind_INBOX_KIND_QUESTION, true
	case "fyi":
		return lobslawv1.InboxKind_INBOX_KIND_FYI, true
	default:
		return lobslawv1.InboxKind_INBOX_KIND_UNSPECIFIED, false
	}
}
