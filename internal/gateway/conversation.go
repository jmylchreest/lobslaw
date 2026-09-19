package gateway

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/turn"
)

// chatHistoryDefaults chosen so a chatty user gets several rounds of
// context without blowing through the LLM context window. The cap is
// in MESSAGES not turns — a single user→assistant exchange that calls
// 4 tools produces ~10 messages (user + 5 assistants + 4 tool
// results), so 100 covers ~10 multi-tool exchanges before truncation.
// 30 minutes matches the attention span of a back-and-forth before
// context decays into a new conversation.
const (
	defaultHistoryMaxMessages = 100
	defaultHistoryTTL         = 30 * time.Minute
	// defaultSessionTail bounds how much of a durable transcript is
	// replayed into a turn. The store retains more than this (see
	// memory.DefaultSessionMaxMessages) so that search and export have
	// something to work with; only the recent tail goes to the model.
	defaultSessionTail = 100
)

// ErrSessionUnavailable marks a durable-store failure the channel can
// safely ride out on its in-memory cache — principally a write
// attempted on a raft follower, which is expected rather than broken.
// The node-side adapter maps memory.ErrNotLeader onto this so the
// gateway needn't import the memory package.
var ErrSessionUnavailable = errors.New("gateway: durable session store unavailable")

// SessionRef identifies one conversation to the durable store.
type SessionRef struct {
	Channel   string
	ChannelID string
	UserID    string
}

// SessionStore is the durable transcript interface. Implemented by an
// adapter over memory.SessionService; kept as a gateway-local
// interface (like ChannelStateStore) so channels stay decoupled from
// the memory package and are trivially fakeable in tests.
type SessionStore interface {
	// LoadTranscript returns the running summary plus the messages
	// that follow it, capped at n verbatim messages (0 = all). An
	// absent conversation returns an empty transcript and no error.
	LoadTranscript(ctx context.Context, ref SessionRef, n int) (Transcript, error)
	Append(ctx context.Context, ref SessionRef, turnID string, msgs []turn.Message) error
	Forget(ctx context.Context, ref SessionRef) error
}

// Transcript is a conversation prepared for a turn.
type Transcript struct {
	// Summary stands in for the compacted head of the conversation.
	// Empty until the thread is long enough to need compacting.
	Summary string
	// Messages are the verbatim tail, oldest first.
	Messages []turn.Message
}

// SessionCompactor folds aged-out conversation into the running
// summary. Optional — nil means no compaction, and long threads lose
// their head to the context budget instead of being summarised.
type SessionCompactor interface {
	MaybeCompact(ctx context.Context, ref SessionRef) (bool, error)
}

// conversationLog is what channels actually talk to: a durable store
// fronted by an in-memory cache.
//
// The cache is not an optimisation, it's the degraded mode. Session
// writes are leader-only (no forwarding layer exists in this
// codebase), so a turn handled on a follower cannot persist. Rather
// than fail the user's message, the cache keeps that conversation
// coherent for the life of the process and the durable copy resumes
// when leadership settles.
//
// With no durable store wired at all — a gateway-only node, or a
// test — this degrades to exactly the old in-memory behaviour.
type conversationLog struct {
	durable   SessionStore
	compactor SessionCompactor
	cache     *chatHistory
	log       *slog.Logger
	tail      int
}

// ConversationConfig tunes what a channel replays and how much the
// degraded-mode cache holds. Zero values take the defaults.
type ConversationConfig struct {
	// TailMessages caps how many stored messages are read per turn.
	TailMessages int
	// CacheMessages and CacheTTL size the in-memory buffer used when
	// the durable store is unavailable (typically a raft follower).
	CacheMessages int
	CacheTTL      time.Duration
}

func newConversationLog(durable SessionStore, compactor SessionCompactor, cfg ConversationConfig, logger *slog.Logger) *conversationLog {
	if logger == nil {
		logger = slog.Default()
	}
	tail := cfg.TailMessages
	if tail <= 0 {
		tail = defaultSessionTail
	}
	return &conversationLog{
		durable:   durable,
		compactor: compactor,
		cache:     newChatHistory(cfg.CacheMessages, cfg.CacheTTL),
		log:       logger,
		tail:      tail,
	}
}

// Load returns prior conversation context for a turn.
//
// A durable read that succeeds but comes back empty falls through to
// the cache: that's the follower case, where earlier turns in this
// conversation only ever made it into memory. Preferring the cache
// there is what stops a leadership change mid-conversation from
// looking like amnesia to the user.
func (c *conversationLog) Load(ctx context.Context, ref SessionRef) Transcript {
	if c.durable != nil {
		t, err := c.durable.LoadTranscript(ctx, ref, c.tail)
		switch {
		case err != nil:
			c.log.Warn("session: durable load failed; falling back to in-memory history",
				"err", err, "channel", ref.Channel, "channel_id", ref.ChannelID)
		case len(t.Messages) > 0 || t.Summary != "":
			return t
		}
	}
	return Transcript{Messages: c.cache.Load(cacheKey(ref))}
}

// Append records a turn in both tiers. Never returns an error: losing
// history is bad, but failing a turn the agent already completed —
// after tools ran and the user got a reply — is worse.
func (c *conversationLog) Append(ctx context.Context, ref SessionRef, turnID string, msgs []turn.Message) {
	if len(msgs) == 0 {
		return
	}
	c.cache.Append(cacheKey(ref), msgs...)
	if c.durable == nil {
		return
	}
	if err := c.durable.Append(ctx, ref, turnID, msgs); err != nil {
		if errors.Is(err, ErrSessionUnavailable) {
			c.log.Debug("session: not persisted (node is not the raft leader); in-memory history retained",
				"channel", ref.Channel, "channel_id", ref.ChannelID, "turn_id", turnID)
			return
		}
		c.log.Warn("session: durable append failed; in-memory history retained",
			"err", err, "channel", ref.Channel, "channel_id", ref.ChannelID, "turn_id", turnID)
		return
	}
	c.compact(ctx, ref)
}

// compact runs compaction off the reply path. The user has already
// been answered by the time a turn is appended, and summarising is an
// LLM round-trip — making them wait for it would trade the tokens we
// saved for latency they can feel.
//
// context.WithoutCancel because the caller's context dies with the
// HTTP request or the Telegram update; the compaction it triggered
// should still finish.
func (c *conversationLog) compact(ctx context.Context, ref SessionRef) {
	if c.compactor == nil {
		return
	}
	go func() {
		bg := context.WithoutCancel(ctx)
		if _, err := c.compactor.MaybeCompact(bg, ref); err != nil {
			if errors.Is(err, ErrSessionUnavailable) {
				return
			}
			c.log.Warn("session: compaction failed; conversation keeps replaying verbatim",
				"err", err, "channel", ref.Channel, "channel_id", ref.ChannelID)
		}
	}()
}

// Forget clears a conversation from both tiers. The cache is always
// cleared even when the durable delete fails, so a user asking to
// forget gets the immediate effect they asked for.
func (c *conversationLog) Forget(ctx context.Context, ref SessionRef) error {
	c.cache.Forget(cacheKey(ref))
	if c.durable == nil {
		return nil
	}
	return c.durable.Forget(ctx, ref)
}

// cacheKey is the in-memory bucket key. Uses the same "<channel>:<id>"
// shape as the durable store so the two tiers partition identically.
func cacheKey(ref SessionRef) string {
	return ref.Channel + ":" + ref.ChannelID
}

// chatHistory is an in-memory rolling buffer of Messages per
// conversation. Ephemeral by design — see conversationLog for how it
// relates to the durable store.
type chatHistory struct {
	mu          sync.Mutex
	buckets     map[string]*historyBucket
	maxMessages int
	ttl         time.Duration
}

type historyBucket struct {
	messages []turn.Message
	lastUsed time.Time
}

func newChatHistory(maxMessages int, ttl time.Duration) *chatHistory {
	if maxMessages <= 0 {
		maxMessages = defaultHistoryMaxMessages
	}
	if ttl <= 0 {
		ttl = defaultHistoryTTL
	}
	return &chatHistory{
		buckets:     make(map[string]*historyBucket),
		maxMessages: maxMessages,
		ttl:         ttl,
	}
}

// Load returns a defensive copy of the conversation's history, or nil
// when the bucket is missing or stale. Loading also refreshes lastUsed
// so active conversations stay warm.
func (h *chatHistory) Load(key string) []turn.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.evictStaleLocked()
	b, ok := h.buckets[key]
	if !ok {
		return nil
	}
	b.lastUsed = time.Now()
	out := make([]turn.Message, len(b.messages))
	copy(out, b.messages)
	return out
}

// Append adds a turn's messages to the buffer, truncating the oldest
// entries once the total exceeds maxMessages. Safe to call with any
// number of messages — a single turn commonly produces user+assistant
// +tool triples.
func (h *chatHistory) Append(key string, msgs ...turn.Message) {
	if len(msgs) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	b, ok := h.buckets[key]
	if !ok {
		b = &historyBucket{}
		h.buckets[key] = b
	}
	b.messages = append(b.messages, msgs...)
	if len(b.messages) > h.maxMessages {
		drop := len(b.messages) - h.maxMessages
		b.messages = append(b.messages[:0], b.messages[drop:]...)
	}
	b.lastUsed = time.Now()
}

// Forget drops the conversation's history.
func (h *chatHistory) Forget(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.buckets, key)
}

// evictStaleLocked clears buckets that haven't been touched in TTL.
// Called from the load path so idle conversations shed memory
// naturally without a background goroutine.
func (h *chatHistory) evictStaleLocked() {
	cutoff := time.Now().Add(-h.ttl)
	for id, b := range h.buckets {
		if b.lastUsed.Before(cutoff) {
			delete(h.buckets, id)
		}
	}
}

// newTurnMessages slices an Agent's resp.Messages down to just the
// messages this turn produced — the user message plus every assistant
// and tool message the loop generated — so callers append only what's
// new to the conversation.
//
// The boundary comes from the agent (resp.TurnStartIndex) rather than
// being recomputed here. The caller cannot derive it: the agent's
// ContextBudget may drop replayed history before it reaches
// resp.Messages, so "one for the system prompt plus the history I
// handed in" lands past the end of the list and silently persists
// nothing.
//
// Out-of-range indices yield nil rather than panicking — a wrong
// answer here costs a lost turn, not a crashed gateway.
func newTurnMessages(all []turn.Message, turnStart int) []turn.Message {
	if turnStart < 0 || turnStart >= len(all) {
		return nil
	}
	out := make([]turn.Message, len(all)-turnStart)
	copy(out, all[turnStart:])
	return out
}
