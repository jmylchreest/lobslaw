package gateway

import (
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// chatHistoryDefaults chosen so a chatty user gets several rounds
// of context without blowing through LLM context. 20 turns covers
// ~10 exchanges including tool-role intermediates; 30 minutes matches
// the attention span of a back-and-forth before context decays
// into a new conversation.
const (
	defaultHistoryMaxTurns = 20
	defaultHistoryTTL      = 30 * time.Minute
)

// chatHistory is an in-memory rolling buffer of Messages per
// Telegram chat_id. Ephemeral by design: conversation context lost
// on process restart, which is acceptable for MVP. Persistent
// recall is the job of the episodic-memory tool, not this buffer.
type chatHistory struct {
	mu       sync.Mutex
	buckets  map[int64]*historyBucket
	maxTurns int
	ttl      time.Duration
}

type historyBucket struct {
	messages []compute.Message
	lastUsed time.Time
}

func newChatHistory(maxTurns int, ttl time.Duration) *chatHistory {
	if maxTurns <= 0 {
		maxTurns = defaultHistoryMaxTurns
	}
	if ttl <= 0 {
		ttl = defaultHistoryTTL
	}
	return &chatHistory{
		buckets:  make(map[int64]*historyBucket),
		maxTurns: maxTurns,
		ttl:      ttl,
	}
}

// Load returns a defensive copy of the chat's history, or nil when
// the bucket is missing or stale. Loading also refreshes lastUsed
// so active conversations stay warm.
func (h *chatHistory) Load(chatID int64) []compute.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.evictStaleLocked()
	b, ok := h.buckets[chatID]
	if !ok {
		return nil
	}
	b.lastUsed = time.Now()
	out := make([]compute.Message, len(b.messages))
	copy(out, b.messages)
	return out
}

// Append adds a turn's messages to the chat's buffer, truncating
// the oldest entries once the total exceeds maxTurns. Safe to call
// with any number of messages — a single turn commonly produces
// user+assistant+tool triples.
func (h *chatHistory) Append(chatID int64, msgs ...compute.Message) {
	if len(msgs) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	b, ok := h.buckets[chatID]
	if !ok {
		b = &historyBucket{}
		h.buckets[chatID] = b
	}
	b.messages = append(b.messages, msgs...)
	if len(b.messages) > h.maxTurns {
		drop := len(b.messages) - h.maxTurns
		b.messages = append(b.messages[:0], b.messages[drop:]...)
	}
	b.lastUsed = time.Now()
}

// Forget drops the chat's history. Exposed so a future /reset
// command can clear context mid-conversation.
func (h *chatHistory) Forget(chatID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.buckets, chatID)
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
