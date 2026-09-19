package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/promptgen"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// askBotTimeout bounds one delegated turn.
//
// Shorter than a top-level turn's budget: the caller is holding a
// conversation open while this runs, and a person waiting on the coordinator
// has no idea a second bot is even involved. A question that cannot be
// answered inside ninety seconds is one that should have been handed
// over with tell_bot.
const askBotTimeout = 90 * time.Second

// BotRegistry is the slice of memory.BotService the builtins use.
type BotRegistry interface {
	Get(ctx context.Context, id string) (*lobslawv1.BotRecord, error)
	List(ctx context.Context) ([]*lobslawv1.BotRecord, error)
	Put(ctx context.Context, rec *lobslawv1.BotRecord, expectedRevision uint64) (*lobslawv1.BotRecord, error)
}

// AskRunner starts a delegated turn. Agent satisfies this.
type AskRunner interface {
	RunToolCallLoop(ctx context.Context, req compute.ProcessMessageRequest) (*compute.ProcessMessageResponse, error)
}

// BotConfig wires the bot builtins.
type BotConfig struct {
	Registry BotRegistry
	Resolver compute.BotResolver
	// Runner starts the delegated turn ask_bot waits on. Nil leaves
	// ask_bot unregistered — a node that cannot run a turn should not
	// advertise a tool that says it can.
	Runner AskRunner
	// Inbox is where an ask/answer pair is journalled. Nil skips the
	// journal; the answer still returns.
	Inbox InboxService
}

// RegisterBotBuiltins installs the team-management and delegation
// builtins.
func RegisterBotBuiltins(b *Builtins, cfg BotConfig) error {
	if cfg.Registry == nil {
		return errors.New("bot builtins: Registry required")
	}
	if err := b.Register("bot_list", newBotListHandler(cfg.Registry)); err != nil {
		return err
	}
	if err := b.Register("bot_create", newBotCreateHandler(cfg.Registry)); err != nil {
		return err
	}
	if err := b.Register("bot_update", newBotUpdateHandler(cfg.Registry)); err != nil {
		return err
	}
	if cfg.Runner == nil {
		return nil
	}
	if err := b.Register("ask_bot", newAskBotHandler(cfg.Runner, cfg.Resolver, cfg.Inbox)); err != nil {
		return err
	}
	return b.Register("tell_bot", newTellBotHandler(cfg.Inbox, cfg.Resolver))
}

// BotToolDefs returns the LLM-facing registrations.
//
// ask_bot is included unconditionally even though its handler is
// skipped without a runner: the registry decides what is advertised,
// and a node that registers the def without the handler fails at
// invoke rather than silently. Callers that cannot run turns do not
// register any of this.
func BotToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "bot_list",
			Path:        compute.BuiltinScheme + "bot_list",
			Description: "List the bots on this cluster — the coordinator and every specialist. Returns id, display name, description, whether it is enabled, which tools it may use and which other bots it may message. Use it before handing work to somebody, to check who exists and what they do. Present as a markdown table.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {},
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "bot_create",
			Path:        compute.BuiltinScheme + "bot_create",
			Description: "Create a new specialist bot. Use when the user asks for a bot to handle an area of work — 'make me a devops bot that watches the cluster', 'I want a marketing bot'. Pass id (lowercase slug, immutable, becomes its identity), display_name, description (one line, so others know who to hand work to) and instructions (its standing brief: what it is for and how it should work). Optional tools (comma-separated allowlist; omit to give it everything this node has) and may_message (comma-separated bot ids it may ask or hand work to; omit for none). A new bot starts enabled. Creating a bot with a narrow tool allowlist is the way to limit what it can do — a tool left out is one it is never even shown.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"id":           {"type": "string", "description": "Lowercase slug, letters/digits/hyphens. Immutable."},
					"display_name": {"type": "string", "description": "Human-readable name, e.g. \"Engineering\"."},
					"description":  {"type": "string", "description": "One line: what this bot is for."},
					"instructions": {"type": "string", "description": "Standing brief. Its role, not a task."},
					"tools":        {"type": "string", "description": "Comma-separated tool allowlist. Omit for every tool this node has."},
					"may_message":  {"type": "string", "description": "Comma-separated bot ids this bot may ask or hand work to."}
				},
				"required": ["id", "display_name", "instructions"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskCommunicating,
		},
		{
			Name:        "bot_update",
			Path:        compute.BuiltinScheme + "bot_update",
			Description: "Change an existing bot: its brief, description, display name, tool allowlist, messaging edges, or whether it is enabled. Pass bot_id plus only the fields you are changing; anything you omit is left alone. Use enabled=false to stop a bot working without deleting it or its history. You cannot change a bot's id, and you cannot promote one to coordinator.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"bot_id":       {"type": "string", "description": "Which bot to change."},
					"display_name": {"type": "string"},
					"description":  {"type": "string"},
					"instructions": {"type": "string", "description": "Replaces the standing brief."},
					"tools":        {"type": "string", "description": "Comma-separated allowlist. Replaces the existing one."},
					"may_message":  {"type": "string", "description": "Comma-separated bot ids. Replaces the existing list."},
					"enabled":      {"type": "boolean"}
				},
				"required": ["bot_id"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskCommunicating,
		},
		{
			Name:        "tell_bot",
			Path:        compute.BuiltinScheme + "tell_bot",
			Description: "Hand work to another bot WITHOUT waiting. The item is durable: it survives restarts and the recipient works it on its own schedule. Use when you do not need the answer this turn. You may only tell bots you have an edge to. For an answer you need now, use ask_bot.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"bot_id": {"type": "string", "description": "Which bot to tell."},
					"body":   {"type": "string", "description": "The instruction or report."}
				},
				"required": ["bot_id", "body"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskCommunicating,
		},
		{
			Name:        "ask_bot",
			Path:        compute.BuiltinScheme + "ask_bot",
			Description: "Ask another bot a question and WAIT for its answer, which comes back as this tool's result. Use it when you need the answer to carry on — marketing checking a technical claim with engineering before publishing it. The answer costs from your own budget, so ask one clear question rather than opening a conversation. If you do not need the answer right now, or the work will take a while, use inbox_post instead: that hands it over durably and the result comes back to your inbox. You may only ask bots you have an edge to, and a bot you ask cannot ask anyone else.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"bot_id":   {"type": "string", "description": "Which bot to ask."},
					"question": {"type": "string", "description": "One clear, self-contained question. The bot does not see your conversation."}
				},
				"required": ["bot_id", "question"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskCommunicating,
		},
	}
}

func newBotListHandler(reg BotRegistry) compute.BuiltinFunc {
	return func(ctx context.Context, _ map[string]string) ([]byte, int, error) {
		records, err := reg.List(ctx)
		if err != nil {
			return nil, 1, fmt.Errorf("bot_list: %w", err)
		}
		out := make([]map[string]any, 0, len(records))
		for _, rec := range records {
			entry := map[string]any{
				"id":             rec.GetId(),
				"display_name":   rec.GetDisplayName(),
				"description":    rec.GetDescription(),
				"enabled":        rec.GetEnabled(),
				"is_coordinator": rec.GetIsCoordinator(),
			}
			if len(rec.GetTools()) > 0 {
				entry["tools"] = rec.GetTools()
			} else {
				entry["tools"] = "all"
			}
			if len(rec.GetMayMessage()) > 0 {
				entry["may_message"] = rec.GetMayMessage()
			}
			out = append(out, entry)
		}
		body, err := json.Marshal(map[string]any{"bots": out, "count": len(out)})
		return body, 0, err
	}
}

func newBotCreateHandler(reg BotRegistry) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		id := strings.TrimSpace(args["id"])
		if id == "" {
			return nil, 2, errors.New("bot_create: id is required")
		}
		instructions := strings.TrimSpace(args["instructions"])
		if instructions == "" {
			return nil, 2, errors.New("bot_create: instructions are required; a bot with no brief is the assistant with a different name")
		}
		owner, group := callerScope(ctx, reg)
		rec := &lobslawv1.BotRecord{
			Id:           id,
			DisplayName:  strings.TrimSpace(args["display_name"]),
			Description:  strings.TrimSpace(args["description"]),
			Instructions: instructions,
			Tools:        splitList(args["tools"]),
			MayMessage:   splitList(args["may_message"]),
			Enabled:      true,
			// A bot created by the coordinator belongs to the same
			// person, in the same team. Without this it was unowned and
			// in no team, so it was invisible in the console and
			// inaccessible to everyone.
			Owner:     owner,
			GroupId:   group,
			CreatedBy: creatorPrincipal(ctx),
		}
		created, err := reg.Put(ctx, rec, 0)
		if err != nil {
			return nil, 1, fmt.Errorf("bot_create: %w", err)
		}
		body, err := json.Marshal(map[string]any{
			"id":           created.GetId(),
			"display_name": created.GetDisplayName(),
			"enabled":      created.GetEnabled(),
			"note":         "created; it has its own memory, its own inbox and its own personality overlay",
		})
		return body, 0, err
	}
}

func newBotUpdateHandler(reg BotRegistry) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		id := strings.TrimSpace(args["bot_id"])
		if id == "" {
			return nil, 2, errors.New("bot_update: bot_id is required")
		}
		current, err := reg.Get(ctx, id)
		if err != nil {
			return nil, 1, fmt.Errorf("bot_update: %w", err)
		}
		// Read-modify-write on the record we just read, so an omitted
		// field is left alone rather than cleared. The revision check
		// in the registry is what makes that safe against a concurrent
		// edit.
		next := current
		if v, ok := args["display_name"]; ok && strings.TrimSpace(v) != "" {
			next.DisplayName = strings.TrimSpace(v)
		}
		if v, ok := args["description"]; ok && strings.TrimSpace(v) != "" {
			next.Description = strings.TrimSpace(v)
		}
		if v, ok := args["instructions"]; ok && strings.TrimSpace(v) != "" {
			next.Instructions = strings.TrimSpace(v)
		}
		if v, ok := args["tools"]; ok {
			next.Tools = splitList(v)
		}
		if v, ok := args["may_message"]; ok {
			next.MayMessage = splitList(v)
		}
		if v, ok := args["enabled"]; ok && strings.TrimSpace(v) != "" {
			enabled, perr := strconv.ParseBool(strings.TrimSpace(v))
			if perr != nil {
				return nil, 2, fmt.Errorf("bot_update: enabled must be true or false, got %q", v)
			}
			next.Enabled = enabled
		}

		updated, err := reg.Put(ctx, next, current.GetRevision())
		if err != nil {
			return nil, 1, fmt.Errorf("bot_update: %w", err)
		}
		body, err := json.Marshal(map[string]any{
			"id":       updated.GetId(),
			"enabled":  updated.GetEnabled(),
			"revision": updated.GetRevision(),
		})
		return body, 0, err
	}
}

// newAskBotHandler runs a delegated turn and returns its reply.
//
// Five constraints, and the first two are what keep delegation from
// being a way to spend without limit:
//
//   - The child draws on the CALLER's budget, so a tree of asks is
//     bounded by the one number the top-level turn was authorised for
//     however the tree is shaped.
//   - The child gets a profile with ask_bot removed, so a second hop
//     is unexpressible rather than disallowed. One level, and a fork
//     bomb is not a thing that can be typed.
//   - The edge must be declared. The graph is validated acyclic on
//     write, so there is no cycle to detect here.
//   - The child gets its own turn id and writes no session of the
//     caller's, keeping the one-writer-per-conversation invariant.
//   - It is bounded in time, because somebody is waiting.
func newAskBotHandler(runner AskRunner, bots compute.BotResolver, inbox InboxService) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		me, err := callerBot(ctx)
		if err != nil {
			return nil, 2, fmt.Errorf("ask_bot: %w", err)
		}
		target := strings.TrimSpace(args["bot_id"])
		if target == "" {
			return nil, 2, errors.New("ask_bot: bot_id is required")
		}
		if target == me {
			return nil, 2, errors.New("ask_bot: you cannot ask yourself; answer it")
		}
		question := strings.TrimSpace(args["question"])
		if question == "" {
			return nil, 2, errors.New("ask_bot: question is required")
		}
		if err := checkMayMessage(ctx, bots, me, target); err != nil {
			return nil, 2, fmt.Errorf("ask_bot: %w", err)
		}

		identity, _ := turn.IdentityFrom(ctx)
		profile, err := bots.ResolveBot(ctx, target)
		if err != nil {
			return nil, 1, fmt.Errorf("ask_bot: %w", err)
		}

		budget := compute.BudgetFrom(ctx)
		if budget == nil {
			budget, err = compute.NewTurnBudget(compute.BudgetCaps{})
			if err != nil {
				return nil, 1, fmt.Errorf("ask_bot: %w", err)
			}
		}

		child, cancel := context.WithTimeout(ctx, askBotTimeout)
		defer cancel()
		resp, err := runner.RunToolCallLoop(child, compute.ProcessMessageRequest{
			Bot:    profile.Without("ask_bot"),
			BotID:  target,
			Budget: budget,
			TurnID: identity.TurnID,
			Message: me + " asks:\n\n" + promptgen.WrapContext([]promptgen.ContextBlock{{
				Source:  "ask_bot:" + me,
				Trust:   promptgen.TrustUntrusted,
				Content: question,
			}}),
		})
		if err != nil {
			return nil, 1, fmt.Errorf("ask_bot: %q could not answer: %w", target, err)
		}

		// Never hand back an empty answer. A caller cannot tell "" from
		// "nothing to report" and will invent a reason for it.
		answer := resp.Reply
		if silent := compute.DescribeSilentTurn(resp); silent != "" {
			answer = silent
		}
		journalAsk(ctx, inbox, me, target, question, answer)

		body, jerr := json.Marshal(map[string]any{
			"bot":    target,
			"answer": answer,
		})
		return body, 0, jerr
	}
}

// journalAsk records a synchronous exchange in both inboxes.
//
// Every inter-bot interaction is journalled, sync or async, so the GUI
// has one complete picture rather than one that silently omits
// whichever half happened to be fast. Both items land already
// resolved, so the queue sitting off the synchronous path costs the
// caller nothing.
//
// Failures are swallowed: a journal that could fail an answered
// question would make bookkeeping able to break the thing it records.
func journalAsk(ctx context.Context, inbox InboxService, from, to, question, answer string) {
	if inbox == nil {
		return
	}
	asked, err := inbox.Post(ctx, &lobslawv1.BotInboxItem{
		Recipient: to,
		Sender:    "bot:" + from,
		Kind:      lobslawv1.InboxKind_INBOX_KIND_QUESTION,
		Body:      question,
	})
	if err != nil {
		return
	}
	_, _ = inbox.Resolve(ctx, to, asked.GetId(), memory.InboxOutcome{
		Result: answer, MaxAttempts: 1,
	})
	answered, err := inbox.Post(ctx, &lobslawv1.BotInboxItem{
		Recipient:     from,
		Sender:        "bot:" + to,
		Kind:          lobslawv1.InboxKind_INBOX_KIND_ANSWER,
		Body:          answer,
		CorrelationId: asked.GetId(),
	})
	if err != nil {
		return
	}
	// Already answered — the caller has it. Resolved immediately so the
	// drain does not run a turn telling the bot something it just read.
	_, _ = inbox.Resolve(ctx, from, answered.GetId(), memory.InboxOutcome{
		Result: "delivered inline as the answer to ask_bot", MaxAttempts: 1,
	})
}

// creatorPrincipal names who proposed a bot, from turn identity.
func newTellBotHandler(inbox InboxService, bots compute.BotResolver) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		if inbox == nil {
			return nil, 1, errors.New("tell_bot: inbox is not wired")
		}
		turnIdentity, _ := turn.IdentityFrom(ctx)
		me, err := callerBot(ctx)
		if err != nil {
			return nil, 2, fmt.Errorf("tell_bot: %w", err)
		}
		target := strings.TrimSpace(args["bot_id"])
		if target == "" {
			return nil, 2, errors.New("tell_bot: bot_id is required")
		}
		if target == me {
			return nil, 2, errors.New("tell_bot: you cannot tell yourself; just do the thing")
		}
		if err := checkMayMessage(ctx, bots, me, target); err != nil {
			return nil, 2, fmt.Errorf("tell_bot: %w", err)
		}
		body := args["body"]
		if strings.TrimSpace(body) == "" {
			return nil, 2, errors.New("tell_bot: body is required")
		}
		item, err := inbox.Post(ctx, &lobslawv1.BotInboxItem{
			Recipient:   target,
			Sender:      "bot:" + me,
			RequestedBy: requesterLabel(turnIdentity),
			Kind:        lobslawv1.InboxKind_INBOX_KIND_TASK,
			Body:        body,
		})
		if err != nil {
			return nil, 1, fmt.Errorf("tell_bot: %w", err)
		}
		out, err := json.Marshal(map[string]any{
			"id":        item.GetId(),
			"recipient": item.GetRecipient(),
			"status":    memory.InboxStatusName(item.GetStatus()),
			"note":      "queued; the recipient works it on its own schedule",
		})
		return out, 0, err
	}
}

func requesterLabel(id turn.Identity) string {
	if p := id.Principal.String(); p != "" {
		return p
	}
	if id.BotID != "" {
		return "bot:" + id.BotID
	}
	return ""
}

// callerScope is the owner and team a new bot inherits from whoever
// asked for it.
//
// A bot created by the coordinator takes the coordinator's owner and
// team; one created by a person takes that person. Group is empty for
// a person because the registry does not own team resolution — the
// console's team-ensure fills it in.
func callerScope(ctx context.Context, reg BotRegistry) (owner, group string) {
	id, ok := turn.IdentityFrom(ctx)
	if !ok {
		return "", ""
	}
	p := id.Principal
	if p.IsBot() {
		botID := strings.TrimPrefix(p.String(), identity.KindBot+":")
		if rec, err := reg.Get(ctx, botID); err == nil && rec != nil {
			return strings.TrimSpace(rec.GetOwner()), strings.TrimSpace(rec.GetGroupId())
		}
		return "", ""
	}
	if strings.HasPrefix(p.String(), identity.KindUser+":") {
		return p.String(), ""
	}
	return "", ""
}

func creatorPrincipal(ctx context.Context) string {
	identity, ok := turn.IdentityFrom(ctx)
	if !ok {
		return "system"
	}
	if p := identity.Principal.String(); p != "" {
		return p
	}
	return "system"
}

func splitList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
