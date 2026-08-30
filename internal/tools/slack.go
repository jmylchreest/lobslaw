package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// SlackReader is the Slack read surface the agent can reach.
//
// An interface here, implemented by the gateway's SlackHandler, because
// the dependency only runs one way: gateway imports compute. Defining
// the question here and letting the channel answer it is the same shape
// as compute.CrossOwnerAuthorizer, and it keeps the builtins testable without a
// workspace.
//
// Implementations MUST enforce the operator's allowed_channels on every
// call. The allowlist governing only inbound events would mean the
// agent can be stopped from HEARING a conversation but not from going
// and reading it.
type SlackReader interface {
	ReadConversation(ctx context.Context, ref string, limit int) ([]SlackTranscriptMessage, error)
	ReadThread(ctx context.Context, ref, ts string, limit int) ([]SlackTranscriptMessage, error)
	SearchConversations(ctx context.Context, query string, refs []string, limit int) ([]SlackTranscriptMessage, error)
}

// SlackTranscriptMessage mirrors the gateway's shape. Duplicated rather
// than shared to keep the import direction one-way; the field tags are
// what the model sees, so they are the contract.
type SlackTranscriptMessage struct {
	Channel  string `json:"channel"`
	User     string `json:"user,omitempty"`
	Text     string `json:"text"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts,omitempty"`
}

// SlackToolConfig wires the slack_* builtins. A nil Reader leaves them
// unregistered, so a node with no Slack channel does not advertise
// tools that cannot work.
type SlackToolConfig struct {
	Reader SlackReader
}

// RegisterSlackBuiltins installs slack_read_channel and slack_search.
//
// NOTE for the policy seed: these are listed in wire_seeds.go's
// noSeedTools, so they get neither a default-allow nor a default-deny.
// Builtins are otherwise seeded default-ALLOW on the grounds that they
// are lobslaw-curated with well-understood blast radius — which is true
// of read_file and not of a tool that reads a company's Slack. Reading
// history is an operator decision, so permission lives entirely in
// [[policy.rules]].
func RegisterSlackBuiltins(b *Builtins, cfg SlackToolConfig) error {
	if cfg.Reader == nil {
		return errors.New("slack tools: Reader required to register builtins")
	}
	if err := b.Register("slack_read_channel", newSlackReadHandler(cfg.Reader)); err != nil {
		return err
	}
	return b.Register("slack_search", newSlackSearchHandler(cfg.Reader))
}

func newSlackReadHandler(r SlackReader) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		channel := strings.TrimSpace(args["channel"])
		if channel == "" {
			return nil, 2, errors.New("channel is required (a channel id like C0123ABC, or #name)")
		}
		limit := clampArg(args["limit"], 50, 200)

		var (
			msgs []SlackTranscriptMessage
			err  error
		)
		if ts := strings.TrimSpace(args["thread_ts"]); ts != "" {
			msgs, err = r.ReadThread(ctx, channel, ts, limit)
		} else {
			msgs, err = r.ReadConversation(ctx, channel, limit)
		}
		if err != nil {
			// Surfaced verbatim: "not in allowed_channels" is a
			// configuration answer the operator needs to see, and one
			// the model should not retry its way around.
			return nil, 1, err
		}
		if len(msgs) == 0 {
			return []byte("No readable messages in that conversation."), 0, nil
		}
		return renderSlackMessages(msgs)
	}
}

func newSlackSearchHandler(r SlackReader) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		query := strings.TrimSpace(args["query"])
		if query == "" {
			return nil, 2, errors.New("query is required")
		}
		limit := clampArg(args["limit"], 10, 25)

		var refs []string
		if raw := strings.TrimSpace(args["channels"]); raw != "" {
			for part := range strings.SplitSeq(raw, ",") {
				if p := strings.TrimSpace(part); p != "" {
					refs = append(refs, p)
				}
			}
		}
		hits, err := r.SearchConversations(ctx, query, refs, limit)
		if err != nil {
			return nil, 1, err
		}
		if len(hits) == 0 {
			return fmt.Appendf(nil, "Nothing in the readable Slack history matches %q.", query), 0, nil
		}
		return renderSlackMessages(hits)
	}
}

// renderSlackMessages returns compact JSON. JSON rather than prose
// because the ts is a handle the agent needs back verbatim to read a
// thread, and prose invites it to paraphrase one.
func renderSlackMessages(msgs []SlackTranscriptMessage) ([]byte, int, error) {
	out, err := json.Marshal(map[string]any{
		"count":    len(msgs),
		"messages": msgs,
	})
	if err != nil {
		return nil, 1, err
	}
	return out, 0, nil
}

// SlackToolDefs are the ToolDefs to register alongside the builtins.
func SlackToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "slack_read_channel",
			Path:        compute.BuiltinScheme + "slack_read_channel",
			Description: "Read recent messages from a Slack conversation the operator has allowed. Pass channel as a channel id (C0123ABC) or #name. Set thread_ts to read one thread instead of the channel. Returns messages oldest-first with their ts, which you can pass back as thread_ts to read a thread. Only conversations in the operator's allowed_channels can be read; anything else is refused.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"channel": {"type": "string", "description": "Channel id (C0123ABC) or #name."},
					"thread_ts": {"type": "string", "description": "Optional. Read this thread instead of the channel."},
					"limit": {"type": "integer", "description": "Messages to return (1-200). Default 50."}
				},
				"required": ["channel"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskCommunicating,
		},
		{
			Name:        "slack_search",
			Path:        compute.BuiltinScheme + "slack_search",
			Description: "Search recent Slack history for a phrase. Case-insensitive substring match over the last few hundred messages of each conversation — NOT a full-workspace search, so treat a miss as \"not in recent history\" rather than \"never said\". Pass channels as a comma-separated list of ids or #names; omit it only when the operator has listed explicit channels. Each hit reports its channel and ts, which you can pass to slack_read_channel to read the surrounding thread.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"query": {"type": "string", "description": "Phrase to look for."},
					"channels": {"type": "string", "description": "Comma-separated channel ids or #names to search. Omit to search every conversation the operator listed."},
					"limit": {"type": "integer", "description": "Hits to return (1-25). Default 10."}
				},
				"required": ["query"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskCommunicating,
		},
	}
}
