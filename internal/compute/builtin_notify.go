package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// TelegramNotifier is the channel-side push interface the
// notify_telegram builtin calls into. The Telegram handler in
// internal/gateway satisfies it via Send(chatID, text). Defined
// here to avoid a compute → gateway import cycle.
type TelegramNotifier interface {
	Send(chatID int64, text string) error
}

// NotifyConfig wires the notify_telegram builtin. Nil notifier
// skips registration — single-channel deployments without Telegram
// won't see this tool.
type NotifyConfig struct {
	Telegram TelegramNotifier
}

// RegisterNotifyBuiltins installs notify_telegram when a notifier
// is supplied. Future channels (Slack, Matrix, etc.) get their own
// builtin here; the current shape stays Telegram-specific because
// that's the only channel exposing a push API today.
func RegisterNotifyBuiltins(b *Builtins, cfg NotifyConfig) error {
	if cfg.Telegram == nil {
		return errors.New("notify builtins: at least one channel notifier required")
	}
	return b.Register("notify_telegram", newNotifyTelegramHandler(cfg.Telegram))
}

func NotifyToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "notify_telegram",
			Path:        BuiltinScheme + "notify_telegram",
			Description: "Send a Telegram message proactively to a specific chat. Use when a scheduled task or commitment fires and needs to deliver its result to a user, OR when you want to follow up on a turn out-of-band. chat_id is the Telegram user/chat ID (numeric). text is the message body. NOTE: in normal in-chat replies you don't need this — your reply text is delivered automatically. notify_telegram is for PROACTIVE messaging where the user isn't currently expecting a reply (commitments, scheduled checks, completion notifications).",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"chat_id": {"type": "string", "description": "Telegram chat ID (numeric, passed as a string for tool-call compatibility)."},
					"text":    {"type": "string", "description": "Message body."}
				},
				"required": ["chat_id", "text"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskCommunicating,
		},
	}
}

func newNotifyTelegramHandler(notifier TelegramNotifier) BuiltinFunc {
	return func(_ context.Context, args map[string]string) ([]byte, int, error) {
		chatIDStr := strings.TrimSpace(args["chat_id"])
		if chatIDStr == "" {
			return nil, 2, errors.New("notify_telegram: chat_id is required")
		}
		chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
		if err != nil {
			return nil, 2, fmt.Errorf("notify_telegram: chat_id must be numeric: %w", err)
		}
		text := args["text"]
		if strings.TrimSpace(text) == "" {
			return nil, 2, errors.New("notify_telegram: text is required")
		}
		if err := notifier.Send(chatID, text); err != nil {
			return nil, 1, fmt.Errorf("notify_telegram: %w", err)
		}
		out, _ := json.Marshal(map[string]any{
			"chat_id":   chatID,
			"delivered": true,
		})
		return out, 0, nil
	}
}
