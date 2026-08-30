package compute

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Summariser defaults, overridable via [compute.context].
const (
	// DefaultCompactMaxCompletionTokens bounds the summariser's
	// completion. The prompt asks for prose well under this; the cap
	// is a backstop against a model that starts transcribing.
	DefaultCompactMaxCompletionTokens = 1024
	// DefaultCompactToolResultBytes is how much of each tool result
	// the summariser reads.
	DefaultCompactToolResultBytes = 400
)

// llmSummarizer implements ConversationSummarizer against an
// LLMProvider — in practice the RoleSummariser client, so operators
// can point compaction at a cheap model while turns run on a better
// one.
type llmSummarizer struct {
	provider LLMProvider
	model    string
	cfg      SummarizerConfig
}

// SummarizerConfig tunes the summariser call. Zero values take the
// package defaults.
type SummarizerConfig struct {
	// MaxCompletionTokens caps what the model may generate.
	MaxCompletionTokens int
	// ToolResultBytes caps how much of each tool result it reads.
	ToolResultBytes int
	// ExtraInstructions are appended to the built-in prompt so a
	// deployment can name what it must never lose.
	ExtraInstructions string
}

// NewLLMSummarizer wires a summariser to a provider. Returns nil when
// no provider is available, which switches compaction off rather than
// failing turns.
func NewLLMSummarizer(provider LLMProvider, model string, cfg SummarizerConfig) ConversationSummarizer {
	if provider == nil {
		return nil
	}
	if cfg.MaxCompletionTokens <= 0 {
		cfg.MaxCompletionTokens = DefaultCompactMaxCompletionTokens
	}
	if cfg.ToolResultBytes <= 0 {
		cfg.ToolResultBytes = DefaultCompactToolResultBytes
	}
	return &llmSummarizer{provider: provider, model: model, cfg: cfg}
}

// summarizerSystemPrompt is deliberately specific about what to keep.
//
// A generic "summarise this" produces narration — "the user asked
// about deployment and the assistant explained the options" — which
// is useless on replay: it records that a topic occurred without
// preserving the answer. What the next turn actually needs is the
// decisions, facts and commitments, in a form the model can act on
// without seeing the original messages.
const summarizerSystemPrompt = `You maintain a running summary of a conversation between a user and an AI assistant.

You will be given the summary so far (possibly empty) and the next batch of messages that are aging out of the conversation. Return an updated summary that folds the new messages into the old summary.

Keep, in order of priority:
- Facts about the user: names, preferences, constraints, environment details, anything they corrected you on.
- Decisions made and their reasoning.
- Commitments: what the assistant said it would do, and whether it did.
- Unresolved threads: open questions, things deferred, known problems.
- Specific identifiers that would be expensive to rediscover: file paths, IDs, URLs, config values, error messages.

Discard: pleasantries, restated questions, tool mechanics, and anything superseded by a later message.

Write plain prose in the third person ("the user prefers…", "the assistant deployed…"). No headings, no bullet lists, no preamble. Never invent detail that is not in the messages. If the new messages add nothing worth keeping, return the previous summary unchanged.`

func (s *llmSummarizer) SummarizeConversation(ctx context.Context, prior string, msgs []Message) (string, error) {
	if len(msgs) == 0 {
		return prior, nil
	}
	var b strings.Builder
	if prior != "" {
		b.WriteString("Summary so far:\n")
		b.WriteString(prior)
		b.WriteString("\n\n")
	} else {
		b.WriteString("There is no summary yet; this is the start of the conversation.\n\n")
	}
	b.WriteString("New messages to fold in:\n")
	for _, m := range msgs {
		b.WriteString(RenderForSummary(m, s.cfg.ToolResultBytes))
	}

	system := summarizerSystemPrompt
	if extra := strings.TrimSpace(s.cfg.ExtraInstructions); extra != "" {
		system += "\n\nAdditional instructions for this deployment:\n" + extra
	}

	resp, err := s.provider.Chat(ctx, ChatRequest{
		Model:       s.model,
		MaxTokens:   s.cfg.MaxCompletionTokens,
		Temperature: 0.2,
		Messages: []Message{
			{Role: "system", Content: system},
			{Role: "user", Content: b.String()},
		},
	})
	if err != nil {
		return "", err
	}
	if resp == nil || strings.TrimSpace(resp.Content) == "" {
		return "", errors.New("summarizer: empty response")
	}
	return strings.TrimSpace(resp.Content), nil
}

// RenderForSummary flattens one message into the transcript the
// summariser reads.
//
// Tool results are truncated hard. The summariser is being asked what
// mattered about an exchange, and 10 MB of grep output does not help
// it answer that — it just costs tokens on a call whose entire
// purpose is to save them.
func RenderForSummary(m Message, maxToolResultBytes int) string {
	var b strings.Builder
	switch m.Role {
	case "tool":
		content := m.Content
		if len(content) > maxToolResultBytes {
			content = content[:maxToolResultBytes] + fmt.Sprintf("… (%d bytes total)", len(m.Content))
		}
		b.WriteString("[tool result] ")
		b.WriteString(content)
	case "assistant":
		if len(m.ToolCalls) > 0 {
			names := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				names = append(names, tc.Name)
			}
			b.WriteString("[assistant called: " + strings.Join(names, ", ") + "] ")
		} else {
			b.WriteString("[assistant] ")
		}
		b.WriteString(m.Content)
	default:
		b.WriteString("[" + m.Role + "] ")
		b.WriteString(m.Content)
	}
	b.WriteString("\n")
	return b.String()
}
