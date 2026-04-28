package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/notify"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Notifier is the channel-agnostic notification dispatch interface
// the `notify` builtin calls into. internal/notify.Service satisfies
// it; tests substitute a fake recorder.
type Notifier interface {
	Send(ctx context.Context, n notify.Notification) error
}

// NotifyConfig wires the notify builtin. Nil Service skips
// registration — deployments without any gateway channel running a
// Sink won't see this tool. Operators with at least one channel
// configured (Telegram, REST, future Slack) get it always.
type NotifyConfig struct {
	Service Notifier
}

// RegisterNotifyBuiltins installs the channel-agnostic `notify`
// builtin. The agent passes a canonical user_id (resolved by the
// channel layer at inbound time, or by the commitment record's
// CreatedFor at fire time); the notify service routes to whichever
// channels that user has bound in their preferences.
func RegisterNotifyBuiltins(b *Builtins, cfg NotifyConfig) error {
	if cfg.Service == nil {
		return errors.New("notify builtins: notify.Service required")
	}
	return b.Register("notify", newNotifyHandler(cfg.Service))
}

// NotifyToolDefs returns the LLM-facing tool registration for
// `notify`. The shape replaces the channel-specific predecessors
// (notify_telegram et al) — operators who want per-channel routing
// get it via the user's preferences bucket, not a per-builtin tool.
func NotifyToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "notify",
			Path:        BuiltinScheme + "notify",
			Description: "Send a proactive message to a user across every channel they're subscribed to. Use ONLY for proactive messaging — commitment fires, scheduled-task results, async research completions, follow-ups on turns out-of-band. For normal in-chat replies, return your reply text directly (the gateway delivers it automatically; calling notify duplicates the message). Pass user_id (canonical user identifier — usually \"owner\" for solo deployments, or the synthetic __user_id arg the agent injects automatically). text is the message body. Optional ttl_seconds (default 300) caps how long the message is allowed to wait before delivery; expired messages drop silently with an audit log.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"user_id":     {"type": "string", "description": "Canonical user id; falls back to the synthetic __user_id from the originating turn when omitted."},
					"text":        {"type": "string", "description": "Message body."},
					"ttl_seconds": {"type": "integer", "description": "Expiry in seconds. Default 300 (5 min). Past this, the message is dropped."}
				},
				"required": ["text"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskCommunicating,
		},
	}
}

func newNotifyHandler(svc Notifier) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		userID := strings.TrimSpace(args["user_id"])
		if userID == "" {
			userID = strings.TrimSpace(args["__user_id"])
		}
		if userID == "" {
			return nil, 2, errors.New("notify: user_id is required (and no synthetic context available)")
		}
		text := args["text"]
		if strings.TrimSpace(text) == "" {
			return nil, 2, errors.New("notify: text is required")
		}

		n := notify.Notification{
			UserID:            userID,
			Body:              text,
			OriginatorChannel: strings.TrimSpace(args["__channel"]),
			OriginatorID:      strings.TrimSpace(args["__chat_id"]),
		}
		if raw := strings.TrimSpace(args["ttl_seconds"]); raw != "" {
			secs, err := parseTTL(raw)
			if err != nil {
				return nil, 2, fmt.Errorf("notify: ttl_seconds: %w", err)
			}
			n.ExpiresAt = time.Now().Add(secs)
		}

		if err := svc.Send(ctx, n); err != nil {
			if errors.Is(err, notify.ErrExpired) {
				return nil, 1, fmt.Errorf("notify: dropped (expired before delivery)")
			}
			if errors.Is(err, notify.ErrUserUnbound) {
				return nil, 1, fmt.Errorf("notify: user %q has no reachable channel addresses; ask the operator to bind one", userID)
			}
			return nil, 1, fmt.Errorf("notify: %w", err)
		}
		out, _ := json.Marshal(map[string]any{
			"user_id":   userID,
			"delivered": true,
		})
		return out, 0, nil
	}
}

func parseTTL(raw string) (time.Duration, error) {
	d, err := time.ParseDuration(raw + "s")
	if err == nil {
		return d, nil
	}
	d, err = time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("must be seconds (\"30\") or duration (\"30s\", \"5m\"): %w", err)
	}
	return d, nil
}
