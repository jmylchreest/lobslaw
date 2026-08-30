package compute

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/turn"
)

// Compaction defaults.
const (
	// DefaultCompactKeepMessages is how many recent messages stay
	// verbatim. Compaction only ever considers what is older than
	// this, so the model always has the immediate exchange in full
	// even right after a compaction runs.
	DefaultCompactKeepMessages = 40
	// DefaultCompactTriggerTokens is how much aged-out material must
	// accumulate before a summariser call is worth making. Too low
	// and every turn pays for an LLM round-trip to fold in two
	// messages; too high and the tail budget drops content before
	// the summary ever captures it.
	DefaultCompactTriggerTokens = 1500
	// DefaultCompactMaxSummaryTokens caps the summary itself. It is
	// prepended to every subsequent turn, so an unbounded summary
	// just recreates the problem it exists to solve.
	DefaultCompactMaxSummaryTokens = 600
)

// ConversationSummarizer folds an aged-out span of conversation into
// a running summary. Separate from memory.Summarizer, which
// consolidates episodic records and also returns an embedding —
// different inputs, different output, no useful shared shape.
type ConversationSummarizer interface {
	// SummarizeConversation returns the new running summary. prior
	// is the existing summary ("" on first compaction); msgs are the
	// messages aging out of verbatim replay.
	SummarizeConversation(ctx context.Context, prior string, msgs []Message) (string, error)
}

// SessionSummaryStore is the slice of the session store compaction
// needs. Narrow on purpose so internal/compute doesn't take a
// dependency on the whole memory service, and so tests can fake it.
type SessionSummaryStore interface {
	// Pending reports what is eligible for compaction: the existing
	// summary, the sequence it covers through, and the highest
	// sequence currently stored.
	Pending(ctx context.Context, ref turn.SessionKey) (summary string, throughSeq, nextSeq uint64, err error)
	// Range returns messages in (afterSeq, throughSeq].
	Range(ctx context.Context, ref turn.SessionKey, afterSeq, throughSeq uint64) ([]Message, error)
	// PutSummary stores a compaction result.
	PutSummary(ctx context.Context, ref turn.SessionKey, summary string, throughSeq uint64) error
	// Title reports the session's current label, and PutTitle sets
	// it. Titles are derived from the summary, so they ride along
	// with compaction rather than needing their own trigger.
	Title(ctx context.Context, ref turn.SessionKey) (string, error)
	PutTitle(ctx context.Context, ref turn.SessionKey, title string) error
}

// CompactorConfig tunes when and how hard compaction runs.
type CompactorConfig struct {
	KeepMessages     int
	TriggerTokens    int
	MaxSummaryTokens int
	// Titler names the conversation from its summary. Nil leaves
	// sessions untitled — searchable, just not labelled.
	Titler Titler
	Logger *slog.Logger
}

// Compactor folds the old end of a conversation into a running
// summary so long threads degrade gracefully instead of losing their
// beginning outright.
//
// It composes the two layers the memory design keeps apart: the store
// is deterministic and never calls an LLM; the summariser is
// interpretive and never writes. The Compactor is the caller that
// orchestrates both.
type Compactor struct {
	store      SessionSummaryStore
	summarizer ConversationSummarizer
	titler     Titler
	log        *slog.Logger

	keepMessages     int
	triggerTokens    int
	maxSummaryTokens int
}

// NewCompactor returns nil when either dependency is missing — a
// deployment without a summariser or a session store simply doesn't
// compact, and callers treat nil as "feature off" rather than
// branching on config.
func NewCompactor(store SessionSummaryStore, summarizer ConversationSummarizer, cfg CompactorConfig) *Compactor {
	if store == nil || summarizer == nil {
		return nil
	}
	c := &Compactor{
		store:            store,
		summarizer:       summarizer,
		titler:           cfg.Titler,
		log:              cfg.Logger,
		keepMessages:     cfg.KeepMessages,
		triggerTokens:    cfg.TriggerTokens,
		maxSummaryTokens: cfg.MaxSummaryTokens,
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	if c.keepMessages <= 0 {
		c.keepMessages = DefaultCompactKeepMessages
	}
	if c.triggerTokens <= 0 {
		c.triggerTokens = DefaultCompactTriggerTokens
	}
	if c.maxSummaryTokens <= 0 {
		c.maxSummaryTokens = DefaultCompactMaxSummaryTokens
	}
	return c
}

// MaybeCompact summarises whatever has aged past the verbatim window,
// if there's enough of it to be worth an LLM call. Safe to call after
// every turn; most calls do nothing.
//
// Returns true when a compaction actually ran.
func (c *Compactor) MaybeCompact(ctx context.Context, key turn.SessionKey) (bool, error) {
	if c == nil {
		return false, nil
	}
	prior, through, next, err := c.store.Pending(ctx, key)
	if err != nil {
		return false, fmt.Errorf("compact: read pending: %w", err)
	}
	// Everything at or below this sequence is old enough to fold in.
	if next <= uint64(c.keepMessages) {
		return false, nil
	}
	boundary := next - 1 - uint64(c.keepMessages)
	if boundary <= through {
		return false, nil
	}

	msgs, err := c.store.Range(ctx, key, through, boundary)
	if err != nil {
		return false, fmt.Errorf("compact: read range: %w", err)
	}
	if len(msgs) == 0 {
		return false, nil
	}
	var tokens int
	for _, m := range msgs {
		tokens += estimateTokens(m)
	}
	if tokens < c.triggerTokens {
		// Not enough has aged out to justify a round-trip. It stays
		// pending and folds into the next compaction instead.
		return false, nil
	}

	summary, err := c.summarizer.SummarizeConversation(ctx, prior, msgs)
	if err != nil {
		return false, fmt.Errorf("compact: summarize: %w", err)
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return false, errors.New("compact: summariser returned nothing")
	}
	summary = truncateToTokens(summary, c.maxSummaryTokens)

	if err := c.store.PutSummary(ctx, key, summary, boundary); err != nil {
		return false, fmt.Errorf("compact: store summary: %w", err)
	}
	c.log.Info("session compacted",
		"channel", key.Channel,
		"channel_id", key.ChannelID,
		"messages_folded", len(msgs),
		"tokens_folded", tokens,
		"through_seq", boundary,
		"summary_bytes", len(summary))

	c.maybeTitle(ctx, key, summary)
	return true, nil
}

// maybeTitle names a conversation once, on its first compaction.
//
// Titling rides on compaction rather than having its own trigger
// because the summary is exactly the input a title needs, and a
// conversation long enough to compact is long enough to be worth
// finding again. Re-titling on every compaction would spend a call
// per compaction to churn a label nobody asked to change.
//
// Failure is logged and swallowed: an untitled session is a cosmetic
// loss, and the compaction that preceded it already succeeded.
func (c *Compactor) maybeTitle(ctx context.Context, key turn.SessionKey, summary string) {
	if c.titler == nil {
		return
	}
	existing, err := c.store.Title(ctx, key)
	if err != nil {
		c.log.Debug("session: title lookup failed", "err", err)
		return
	}
	if strings.TrimSpace(existing) != "" {
		return
	}
	title, err := c.titler.Title(ctx, summary)
	if err != nil {
		c.log.Warn("session: titling failed; conversation stays untitled", "err", err)
		return
	}
	if title == "" {
		return
	}
	if err := c.store.PutTitle(ctx, key, title); err != nil {
		c.log.Warn("session: storing title failed", "err", err)
		return
	}
	c.log.Info("session titled",
		"channel", key.Channel, "channel_id", key.ChannelID, "title", title)
}

// truncateToTokens clips a summary that came back longer than asked
// for. Models overshoot length instructions routinely, and an
// oversized summary is charged on every subsequent turn.
func truncateToTokens(s string, maxTokens int) string {
	maxBytes := maxTokens * 4
	if len(s) <= maxBytes {
		return s
	}
	// Cut at a sentence boundary where one is close to the limit, so
	// the summary doesn't end mid-word.
	cut := truncateAtRune(s, maxBytes)
	if i := strings.LastIndexAny(cut, ".!?\n"); i > maxBytes/2 {
		return cut[:i+1]
	}
	return cut + "…"
}
