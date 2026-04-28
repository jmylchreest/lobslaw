package compute

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

var commitmentIDEntropy = ulid.Monotonic(cryptorand.Reader, 0)

// CommitmentHandlerRef matches node.AgentTurnHandlerRef so
// commitments created via this builtin dispatch through the same
// runCommitmentAsAgentTurn handler operator-defined commitments
// use. Single source of truth, single handler.
const CommitmentHandlerRef = "agent:turn"

// CommitmentConfig wires the commitment_create / list / cancel
// builtins. Same Store + Raft pattern as schedule_*.
type CommitmentConfig struct {
	Store *memory.Store
	Raft  memoryRaftApplier
}

// RegisterCommitmentBuiltins installs commitment_create / list /
// cancel. One-shot due-at scheduling — the right primitive for
// "in 2 minutes message me" or "tomorrow at 9am check the
// weather". Use schedule_create for recurring jobs instead.
func RegisterCommitmentBuiltins(b *Builtins, cfg CommitmentConfig) error {
	if cfg.Store == nil || cfg.Raft == nil {
		return errors.New("commitment builtins: Store + Raft required")
	}
	if err := b.Register("commitment_create", newCommitmentCreateHandler(cfg.Raft)); err != nil {
		return err
	}
	if err := b.Register("commitment_list", newCommitmentListHandler(cfg.Store)); err != nil {
		return err
	}
	return b.Register("commitment_cancel", newCommitmentCancelHandler(cfg.Raft))
}

func CommitmentToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "commitment_create",
			Path:        BuiltinScheme + "commitment_create",
			Description: "Schedule a ONE-SHOT commitment to fire at a specific time in the future. Use for 'in 2 minutes message me', 'tomorrow at 9am check the weather', 'in an hour remind me' — anything that should happen ONCE at a known time. For recurring checks use schedule_create instead. Pass when (relative duration like '2m', '1h', '24h', OR absolute RFC3339 timestamp like '2026-04-25T10:00:00Z') and prompt (the self-instruction the agent runs at fire-time). Optional reason (human-readable purpose). When the commitment fires, the agent runs your prompt — if the user expects a message, your prompt should call notify_telegram(chat_id=ORIGINATOR, text=...) since the firing turn has no chat to reply into.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"when":   {"type": "string", "description": "Relative duration ('2m', '1h', '24h') or RFC3339 timestamp."},
					"prompt": {"type": "string", "description": "Self-instruction fired at the due time."},
					"reason": {"type": "string", "description": "Optional human-readable purpose."}
				},
				"required": ["when", "prompt"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "commitment_list",
			Path:        BuiltinScheme + "commitment_list",
			Description: "List commitments with their due time, prompt, and status. By default only PENDING (not-yet-fired) commitments are returned — already-delivered one-shots are hidden so the user isn't shown stale 'go to bed at 22:00' reminders the morning after. Pass include_history=true to see fired/done commitments too (useful when the user asks 'what did you remind me about yesterday'). Response includes hidden_count so you can mention there's history available without dumping it. Markdown-table renderable.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"include_history": {"type": "boolean", "description": "Include already-fired (done) commitments. Default false — only pending entries are returned."}
				},
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "commitment_cancel",
			Path:        BuiltinScheme + "commitment_cancel",
			Description: "Cancel a pending commitment by id. Use when the user changes their mind ('actually never mind that reminder').",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"id": {"type": "string", "description": "Commitment ULID."}
				},
				"required": ["id"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
	}
}

func newCommitmentCreateHandler(raft memoryRaftApplier) BuiltinFunc {
	return func(_ context.Context, args map[string]string) ([]byte, int, error) {
		when := strings.TrimSpace(args["when"])
		if when == "" {
			return nil, 2, errors.New("commitment_create: when is required (duration or RFC3339)")
		}
		prompt := strings.TrimSpace(args["prompt"])
		if prompt == "" {
			return nil, 2, errors.New("commitment_create: prompt is required")
		}
		userTZ := strings.TrimSpace(args["__user_timezone"])
		dueAt, err := parseWhen(when, userTZ)
		if err != nil {
			return nil, 2, fmt.Errorf("commitment_create: %w", err)
		}
		if dueAt.Before(time.Now()) {
			return nil, 2, fmt.Errorf("commitment_create: due time %s is in the past", formatTimeForUser(dueAt, args))
		}

		id := ulid.MustNew(ulid.Now(), commitmentIDEntropy).String()
		// Auto-capture context from synthetic args injected by
		// agent.runToolCall. The originating user_id flows into
		// the commitment params + into the firing turn's prompt
		// as the notify target — the agent at fire time calls
		// `notify(text=...)` and the system routes via that user's
		// channel preferences. Channel + chat_id stay stored for
		// audit/debug visibility but aren't load-bearing for
		// delivery anymore.
		channel := strings.TrimSpace(args["__channel"])
		chatID := strings.TrimSpace(args["__chat_id"])
		userID := strings.TrimSpace(args["__user_id"])
		if userID != "" {
			prompt = fmt.Sprintf("This commitment was scheduled by user %q. The firing turn has no chat to auto-reply into — to deliver any reply, call notify(text=\"...\") and the system routes to whichever channels that user is subscribed to.\n\nYour task: %s", userID, prompt)
		}
		params := map[string]string{"prompt": prompt}
		if channel != "" {
			params["channel"] = channel
		}
		if chatID != "" {
			params["chat_id"] = chatID
		}
		if userID != "" {
			params["user_id"] = userID
		}
		c := &lobslawv1.AgentCommitment{
			Id:         id,
			DueAt:      timestamppb.New(dueAt),
			Trigger:    "time",
			Reason:     args["reason"],
			Status:     "pending",
			HandlerRef: CommitmentHandlerRef,
			Params:     params,
			CreatedFor: userID,
		}
		entry := &lobslawv1.LogEntry{
			Op: lobslawv1.LogOp_LOG_OP_PUT,
			Id: id,
			Payload: &lobslawv1.LogEntry_Commitment{
				Commitment: c,
			},
		}
		data, err := proto.Marshal(entry)
		if err != nil {
			return nil, 1, fmt.Errorf("commitment_create: marshal: %w", err)
		}
		if _, err := raft.Apply(data, 5*time.Second); err != nil {
			return nil, 1, fmt.Errorf("commitment_create: raft apply: %w", err)
		}
		out, _ := json.Marshal(map[string]any{
			"id":     id,
			"due_at": formatTimeForUser(dueAt, args),
		})
		return out, 0, nil
	}
}

func newCommitmentListHandler(store *memory.Store) BuiltinFunc {
	return func(_ context.Context, args map[string]string) ([]byte, int, error) {
		includeHistory := false
		if raw, ok := args["include_history"]; ok && raw != "" {
			b, err := strconv.ParseBool(raw)
			if err != nil {
				return nil, 2, fmt.Errorf("commitment_list: include_history must be a boolean: %w", err)
			}
			includeHistory = b
		}
		type view struct {
			ID     string `json:"id"`
			DueAt  string `json:"due_at"`
			Reason string `json:"reason,omitempty"`
			Prompt string `json:"prompt,omitempty"`
			Status string `json:"status"`
		}
		var out []view
		hidden := 0
		err := store.ForEach(memory.BucketCommitments, func(_ string, raw []byte) error {
			var c lobslawv1.AgentCommitment
			if err := proto.Unmarshal(raw, &c); err != nil {
				return nil
			}
			// Default-hide anything that's not actively pending. The
			// scheduler stamps fired commitments to "done"; older or
			// custom states (cancelled, errored, etc.) are also
			// uninteresting to the agent's "what's still on the slate"
			// view by default — opt in with include_history.
			if !includeHistory && c.Status != "pending" {
				hidden++
				return nil
			}
			v := view{
				ID:     c.Id,
				Reason: c.Reason,
				Prompt: c.Params["prompt"],
				Status: c.Status,
			}
			if c.DueAt != nil {
				v.DueAt = formatTimeForUser(c.DueAt.AsTime(), args)
			}
			out = append(out, v)
			return nil
		})
		if err != nil {
			return nil, 1, fmt.Errorf("commitment_list: %w", err)
		}
		payload, _ := json.Marshal(map[string]any{
			"count":           len(out),
			"hidden_count":    hidden,
			"include_history": includeHistory,
			"commitments":     out,
		})
		return payload, 0, nil
	}
}

func newCommitmentCancelHandler(raft memoryRaftApplier) BuiltinFunc {
	return func(_ context.Context, args map[string]string) ([]byte, int, error) {
		id := strings.TrimSpace(args["id"])
		if id == "" {
			return nil, 2, errors.New("commitment_cancel: id is required")
		}
		entry := &lobslawv1.LogEntry{
			Op: lobslawv1.LogOp_LOG_OP_DELETE,
			Id: id,
			Payload: &lobslawv1.LogEntry_Commitment{
				Commitment: &lobslawv1.AgentCommitment{Id: id},
			},
		}
		data, err := proto.Marshal(entry)
		if err != nil {
			return nil, 1, fmt.Errorf("commitment_cancel: marshal: %w", err)
		}
		if _, err := raft.Apply(data, 5*time.Second); err != nil {
			return nil, 1, fmt.Errorf("commitment_cancel: raft apply: %w", err)
		}
		out, _ := json.Marshal(map[string]any{"id": id, "cancelled": true})
		return out, 0, nil
	}
}

// parseWhen accepts:
//   - A Go duration ("2m", "1h", "24h") — added to time.Now().
//   - A full RFC3339 timestamp with offset ("2026-04-30T09:00:00+01:00",
//     "...Z") — taken as the absolute UTC moment regardless of TZ.
//   - A naked wall-clock timestamp without offset ("2026-04-30T09:00:00")
//     — interpreted in userTZ (when supplied) or UTC (fallback).
//
// userTZ is the IANA zone the user prefers (resolved via the
// synthetic __user_timezone arg). Empty → UTC, which matches the
// historical behaviour for callers that haven't plumbed TZ context.
//
// All returned times are converted to UTC before being stored —
// the system operates on UTC; only display is timezone-driven.
var rfc3339Hint = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T`)
var rfc3339WithOffset = regexp.MustCompile(`(?:Z|[+-]\d{2}:\d{2})$`)

func parseWhen(when, userTZ string) (time.Time, error) {
	w := strings.TrimSpace(when)
	if rfc3339Hint.MatchString(w) {
		if rfc3339WithOffset.MatchString(w) {
			t, err := time.Parse(time.RFC3339, w)
			if err != nil {
				return time.Time{}, fmt.Errorf("invalid RFC3339 timestamp %q: %w", w, err)
			}
			return t.UTC(), nil
		}
		// Naked wall-clock — interpret in the user's TZ.
		loc := time.UTC
		if userTZ != "" {
			if l, err := time.LoadLocation(userTZ); err == nil {
				loc = l
			}
		}
		t, err := time.ParseInLocation("2006-01-02T15:04:05", w, loc)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid wall-clock timestamp %q (use RFC3339 with offset, or naked YYYY-MM-DDTHH:MM:SS): %w", w, err)
		}
		return t.UTC(), nil
	}
	d, err := time.ParseDuration(w)
	if err != nil {
		return time.Time{}, fmt.Errorf("when %q is neither a Go duration ('2m', '1h') nor a timestamp", w)
	}
	return time.Now().UTC().Add(d), nil
}

