package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// InboxService is the slice of memory.InboxService the builtins use.
// An interface so tests substitute a recorder without raft.
type InboxService interface {
	Journal(ctx context.Context, item *lobslawv1.BotInboxItem) (*lobslawv1.BotInboxItem, error)
	Post(ctx context.Context, item *lobslawv1.BotInboxItem) (*lobslawv1.BotInboxItem, error)
	Get(ctx context.Context, recipient, id string) (*lobslawv1.BotInboxItem, error)
	List(ctx context.Context, recipient string, f memory.InboxFilter) ([]*lobslawv1.BotInboxItem, error)
	Resolve(ctx context.Context, recipient, id string, outcome memory.InboxOutcome) (*lobslawv1.BotInboxItem, error)
}

// InboxConfig wires the inbox builtins.
type InboxConfig struct {
	Service InboxService
	// Bots resolves the caller's profile so inbox_post can check the
	// declared message graph. Nil disables posting to other bots while
	// leaving a bot able to read its own queue.
	Bots compute.BotResolver
}

// RegisterInboxBuiltins installs inbox_list / inbox_read /
// inbox_post / inbox_resolve.
func RegisterInboxBuiltins(b *Builtins, cfg InboxConfig) error {
	if cfg.Service == nil {
		return errors.New("inbox builtins: Service required")
	}
	if err := b.Register("inbox_list", newInboxListHandler(cfg.Service)); err != nil {
		return err
	}
	if err := b.Register("inbox_read", newInboxReadHandler(cfg.Service)); err != nil {
		return err
	}
	if err := b.Register("inbox_resolve", newInboxResolveHandler(cfg.Service)); err != nil {
		return err
	}
	return b.Register("inbox_post", newInboxPostHandler(cfg.Service, cfg.Bots))
}

func InboxToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "inbox_list",
			Path:        compute.BuiltinScheme + "inbox_list",
			Description: "List the items waiting in YOUR OWN inbox — your durable work queue. Use it to answer 'what am I working on', 'anything waiting for me', or to pick up what to do next. Returns items most urgent first (priority, then arrival order) with id, kind, sender, subject, status and, for finished items, the result. Optional status filter ('pending', 'claimed', 'done', 'failed', 'cancelled'; default pending) and limit. You cannot list another bot's inbox — the queue you see is always yours.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"status": {"type": "string", "description": "pending | claimed | done | failed | cancelled | all. Default pending."},
					"limit":  {"type": "integer", "description": "Maximum items to return. Default 20."}
				},
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "inbox_read",
			Path:        compute.BuiltinScheme + "inbox_read",
			Description: "Read one item from your own inbox in full — the whole body, plus the result and error if it has been worked. inbox_list gives you the ids. Use this when a listing's subject is not enough to act on.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"id": {"type": "string", "description": "Item id from inbox_list."}
				},
				"required": ["id"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "inbox_post",
			Path:        compute.BuiltinScheme + "inbox_post",
			Description: "Put an item in ANOTHER bot's inbox. Use it to hand work to a specialist ('deploy the staging branch'), to ask something you do not need answered this instant, or to report an outcome back. The item is durable: it survives restarts, the recipient works it on its own schedule, and the result comes back to your inbox. You may only post to bots you have been given an edge to. For an answer you need WITHIN this turn, use ask_bot instead. Pass bot_id, body, optional subject (one line, defaults to the body's first line), kind ('task', 'question', 'result', 'fyi'; default task) and priority (higher is worked first; default 0).",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"bot_id":   {"type": "string", "description": "Recipient bot id."},
					"subject":  {"type": "string", "description": "One-line summary. Defaults to the body's first line."},
					"body":     {"type": "string", "description": "The instruction, question or report."},
					"kind":     {"type": "string", "description": "task | question | result | fyi. Default task."},
					"priority": {"type": "integer", "description": "Higher is worked first. Default 0."}
				},
				"required": ["bot_id", "body"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskCommunicating,
		},
		{
			Name:        "inbox_resolve",
			Path:        compute.BuiltinScheme + "inbox_resolve",
			Description: "Mark an item in your own inbox done or failed, recording what happened. Normally you do NOT need this — an item you were given to work is resolved automatically when your turn finishes. Use it only to close something out of band: work you had already done, or a task you have decided cannot be completed. Pass id, optional result (what happened) and failed=true if it could not be done.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"id":     {"type": "string", "description": "Item id from inbox_list."},
					"result": {"type": "string", "description": "What happened. Shown in the GUI and to whoever sent it."},
					"failed": {"type": "boolean", "description": "True if the item could not be completed."}
				},
				"required": ["id"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
	}
}

// callerBot returns the bot whose turn this is.
//
// From turn identity, never from an argument. A bot that could name
// its own identity could read another's queue and post work that
// appears to come from somebody else — the same reason the notify
// builtin takes its sender from the turn.
func callerBot(ctx context.Context) (string, error) {
	identity, ok := turn.IdentityFrom(ctx)
	if !ok || identity.BotID == "" {
		return "", errors.New("no bot is taking this turn; the inbox belongs to a bot, and this turn has none")
	}
	return identity.BotID, nil
}

func newInboxListHandler(svc InboxService) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		me, err := callerBot(ctx)
		if err != nil {
			return nil, 2, fmt.Errorf("inbox_list: %w", err)
		}
		filter := memory.InboxFilter{Limit: 20}
		if raw := strings.TrimSpace(args["limit"]); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				return nil, 2, fmt.Errorf("inbox_list: limit must be a positive integer, got %q", raw)
			}
			filter.Limit = n
		}
		status := strings.ToLower(strings.TrimSpace(args["status"]))
		if status == "" {
			status = "pending"
		}
		if status != "all" {
			parsed, ok := parseInboxStatus(status)
			if !ok {
				return nil, 2, fmt.Errorf("inbox_list: unknown status %q; use pending, claimed, done, failed, cancelled or all", status)
			}
			filter.Statuses = []lobslawv1.InboxStatus{parsed}
		}

		items, err := svc.List(ctx, me, filter)
		if err != nil {
			return nil, 1, fmt.Errorf("inbox_list: %w", err)
		}
		out := make([]map[string]any, 0, len(items))
		for _, item := range items {
			out = append(out, inboxSummary(item))
		}
		body, err := json.Marshal(map[string]any{"bot": me, "items": out, "count": len(out)})
		return body, 0, err
	}
}

func newInboxReadHandler(svc InboxService) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		me, err := callerBot(ctx)
		if err != nil {
			return nil, 2, fmt.Errorf("inbox_read: %w", err)
		}
		id := strings.TrimSpace(args["id"])
		if id == "" {
			return nil, 2, errors.New("inbox_read: id is required")
		}
		item, err := svc.Get(ctx, me, id)
		if err != nil {
			return nil, 1, fmt.Errorf("inbox_read: %w", err)
		}
		full := inboxSummary(item)
		full["body"] = item.GetBody()
		full["attempts"] = item.GetAttempts()
		if item.GetError() != "" {
			full["error"] = item.GetError()
		}
		if item.GetSessionId() != "" {
			full["session_id"] = item.GetSessionId()
		}
		body, err := json.Marshal(full)
		return body, 0, err
	}
}

func newInboxResolveHandler(svc InboxService) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		me, err := callerBot(ctx)
		if err != nil {
			return nil, 2, fmt.Errorf("inbox_resolve: %w", err)
		}
		id := strings.TrimSpace(args["id"])
		if id == "" {
			return nil, 2, errors.New("inbox_resolve: id is required")
		}
		outcome := memory.InboxOutcome{Result: args["result"]}
		if strings.EqualFold(strings.TrimSpace(args["failed"]), "true") {
			// MaxAttempts 0 so an explicit failure is final. The bot has
			// decided this cannot be done; retrying it would be the
			// system overruling that.
			outcome.Err = errors.New(firstNonEmpty(args["result"], "the bot reported this could not be completed"))
			outcome.MaxAttempts = 0
		}
		item, err := svc.Resolve(ctx, me, id, outcome)
		if err != nil {
			return nil, 1, fmt.Errorf("inbox_resolve: %w", err)
		}
		body, err := json.Marshal(inboxSummary(item))
		return body, 0, err
	}
}

func newInboxPostHandler(svc InboxService, bots compute.BotResolver) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		turnIdentity, _ := turn.IdentityFrom(ctx)
		me, err := callerBot(ctx)
		if err != nil {
			return nil, 2, fmt.Errorf("inbox_post: %w", err)
		}
		target := strings.TrimSpace(args["bot_id"])
		if target == "" {
			return nil, 2, errors.New("inbox_post: bot_id is required")
		}
		if target == me {
			return nil, 2, errors.New("inbox_post: you cannot post to your own inbox; just do the thing")
		}
		if err := checkMayMessage(ctx, bots, me, target); err != nil {
			return nil, 2, fmt.Errorf("inbox_post: %w", err)
		}
		body := args["body"]
		if strings.TrimSpace(body) == "" {
			return nil, 2, errors.New("inbox_post: body is required")
		}
		kind, ok := parseInboxKind(args["kind"])
		if !ok {
			return nil, 2, fmt.Errorf("inbox_post: unknown kind %q; use task, question, result or fyi", args["kind"])
		}
		priority := 0
		if raw := strings.TrimSpace(args["priority"]); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return nil, 2, fmt.Errorf("inbox_post: priority must be an integer, got %q", raw)
			}
			priority = n
		}

		item, err := svc.Post(ctx, &lobslawv1.BotInboxItem{
			Recipient: target,
			// Sender is stamped here from the turn, never from args.
			Sender: "bot:" + me,
			// And who ASKED, carried forward so the chain survives.
			// Sender becomes another bot after one hop; this stays the
			// person at the start of it. Also from the turn, for the
			// same reason — a requester the model can name is one it
			// can invent.
			RequestedBy: requesterLabel(turnIdentity),
			Kind:        kind,
			Subject:     args["subject"],
			Body:        body,
			Priority:    int32(priority),
		})
		if err != nil {
			return nil, 1, fmt.Errorf("inbox_post: %w", err)
		}
		out, err := json.Marshal(map[string]any{
			"id":        item.GetId(),
			"recipient": item.GetRecipient(),
			"status":    memory.InboxStatusName(item.GetStatus()),
			"note":      "queued; the recipient works it on its own schedule and the outcome comes back to your inbox",
		})
		return out, 0, err
	}
}

// checkMayMessage enforces the declared edge list.
//
// Refusal names the missing edge rather than pretending the bot does
// not exist: the caller is one of the assistant's own agents, the
// target's name came from its own configuration, and an oracle-style
// evasion would only make it retry.
func checkMayMessage(ctx context.Context, bots compute.BotResolver, me, target string) error {
	if bots == nil {
		return errors.New("inter-bot messaging is not wired on this node")
	}
	profile, err := bots.ResolveBot(ctx, me)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", me, err)
	}
	if !profile.MayMessageBot(target) {
		return fmt.Errorf("%q is not in your may_message list, so you cannot reach it; ask the coordinator to grant the edge", target)
	}
	recipient, err := bots.ResolveBot(ctx, target)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", target, err)
	}
	if profile.Owner == "" || profile.Owner != recipient.Owner {
		return errors.New("inter-bot messaging requires the same human owner")
	}
	return nil
}

func inboxSummary(item *lobslawv1.BotInboxItem) map[string]any {
	out := map[string]any{
		"id":       item.GetId(),
		"kind":     memory.InboxKindName(item.GetKind()),
		"sender":   item.GetSender(),
		"subject":  item.GetSubject(),
		"status":   memory.InboxStatusName(item.GetStatus()),
		"priority": item.GetPriority(),
	}
	if item.GetCreatedAt() != nil {
		out["created_at"] = item.GetCreatedAt().AsTime().Format("2006-01-02T15:04:05Z07:00")
	}
	if item.GetResult() != "" {
		out["result"] = item.GetResult()
	}
	if item.GetCorrelationId() != "" {
		out["correlation_id"] = item.GetCorrelationId()
	}
	return out
}

func parseInboxStatus(s string) (lobslawv1.InboxStatus, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "pending":
		return lobslawv1.InboxStatus_INBOX_STATUS_PENDING, true
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

func parseInboxKind(s string) (lobslawv1.InboxKind, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "task":
		return lobslawv1.InboxKind_INBOX_KIND_TASK, true
	case "question":
		return lobslawv1.InboxKind_INBOX_KIND_QUESTION, true
	case "answer":
		return lobslawv1.InboxKind_INBOX_KIND_ANSWER, true
	case "result":
		return lobslawv1.InboxKind_INBOX_KIND_RESULT, true
	case "fyi":
		return lobslawv1.InboxKind_INBOX_KIND_FYI, true
	default:
		return lobslawv1.InboxKind_INBOX_KIND_UNSPECIFIED, false
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
