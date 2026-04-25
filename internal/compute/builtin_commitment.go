package compute

import (
	cryptorand "crypto/rand"
	"context"
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
			Description: "List pending commitments with their due time and prompt. Markdown-table renderable.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {},
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
		dueAt, err := parseWhen(when)
		if err != nil {
			return nil, 2, fmt.Errorf("commitment_create: %w", err)
		}
		if dueAt.Before(time.Now()) {
			return nil, 2, fmt.Errorf("commitment_create: due time %s is in the past", dueAt.Format(time.RFC3339))
		}

		id := ulid.MustNew(ulid.Now(), commitmentIDEntropy).String()
		// Auto-capture channel context from synthetic args injected
		// by agent.runToolCall. The bot doesn't reliably remember
		// to pass chat_id explicitly, so we lift it from the
		// originating turn's context. The firing turn's
		// runCommitmentAsAgentTurn reads params.chat_id back into
		// the agent request's ChannelID, which renders into the
		// Runtime section of the firing turn's system prompt.
		channel := strings.TrimSpace(args["__channel"])
		chatID := strings.TrimSpace(args["__chat_id"])
		// Prefix the stored prompt with an explicit how-to-message-
		// back instruction. Without this, the bot's stored prompt
		// is often generic ("Send a message saying X") and the
		// firing turn generates text that goes nowhere.
		if channel == "telegram" && chatID != "" {
			prompt = fmt.Sprintf("This commitment was scheduled by user in telegram chat %s. To deliver any reply to them you MUST call notify_telegram(chat_id=\"%s\", text=\"...\") — without this call the user will not see your reply, since this turn has no chat to auto-reply into.\n\nYour task: %s", chatID, chatID, prompt)
		}
		params := map[string]string{"prompt": prompt}
		if channel != "" {
			params["channel"] = channel
		}
		if chatID != "" {
			params["chat_id"] = chatID
		}
		c := &lobslawv1.AgentCommitment{
			Id:         id,
			DueAt:      timestamppb.New(dueAt),
			Trigger:    "time",
			Reason:     args["reason"],
			Status:     "pending",
			HandlerRef: CommitmentHandlerRef,
			Params:     params,
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
			"due_at": dueAt.Format(time.RFC3339),
		})
		return out, 0, nil
	}
}

func newCommitmentListHandler(store *memory.Store) BuiltinFunc {
	return func(_ context.Context, _ map[string]string) ([]byte, int, error) {
		type view struct {
			ID     string `json:"id"`
			DueAt  string `json:"due_at"`
			Reason string `json:"reason,omitempty"`
			Prompt string `json:"prompt,omitempty"`
			Status string `json:"status"`
		}
		var out []view
		err := store.ForEach(memory.BucketCommitments, func(_ string, raw []byte) error {
			var c lobslawv1.AgentCommitment
			if err := proto.Unmarshal(raw, &c); err != nil {
				return nil
			}
			v := view{
				ID:     c.Id,
				Reason: c.Reason,
				Prompt: c.Params["prompt"],
				Status: c.Status,
			}
			if c.DueAt != nil {
				v.DueAt = c.DueAt.AsTime().Format(time.RFC3339)
			}
			out = append(out, v)
			return nil
		})
		if err != nil {
			return nil, 1, fmt.Errorf("commitment_list: %w", err)
		}
		payload, _ := json.Marshal(map[string]any{"count": len(out), "commitments": out})
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

// parseWhen accepts either a Go duration ("2m", "1h", "24h") OR an
// RFC3339 timestamp, and returns the absolute due time.
var rfc3339Hint = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T`)

func parseWhen(when string) (time.Time, error) {
	w := strings.TrimSpace(when)
	if rfc3339Hint.MatchString(w) {
		t, err := time.Parse(time.RFC3339, w)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid RFC3339 timestamp %q: %w", w, err)
		}
		return t, nil
	}
	d, err := time.ParseDuration(w)
	if err != nil {
		return time.Time{}, fmt.Errorf("when %q is neither a Go duration ('2m', '1h') nor RFC3339 timestamp", w)
	}
	return time.Now().Add(d), nil
}

// suppress unused import warning when builtins compile in isolation
var _ = strconv.Itoa
