package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

func TestChatHistoryAppendAndLoad(t *testing.T) {
	t.Parallel()
	h := newChatHistory(20, 5*time.Minute)
	h.Append("telegram:42",
		compute.Message{Role: "user", Content: "hi"},
		compute.Message{Role: "assistant", Content: "hey"})
	got := h.Load("telegram:42")
	if len(got) != 2 {
		t.Fatalf("Load = %d; want 2", len(got))
	}
	if got[0].Role != "user" || got[1].Role != "assistant" {
		t.Errorf("order lost: %+v", got)
	}
}

func TestChatHistoryCapsAtMaxTurns(t *testing.T) {
	t.Parallel()
	h := newChatHistory(4, time.Hour)
	for range 10 {
		h.Append("telegram:1", compute.Message{Role: "user", Content: "msg"})
	}
	got := h.Load("telegram:1")
	if len(got) != 4 {
		t.Errorf("cap = %d; want 4", len(got))
	}
}

func TestChatHistoryEvictsStale(t *testing.T) {
	t.Parallel()
	h := newChatHistory(20, 10*time.Millisecond)
	h.Append("telegram:1", compute.Message{Role: "user", Content: "early"})
	time.Sleep(25 * time.Millisecond)
	if got := h.Load("telegram:1"); len(got) != 0 {
		t.Errorf("expired bucket should Load empty; got %d msgs", len(got))
	}
}

func TestChatHistorySeparatesByChat(t *testing.T) {
	t.Parallel()
	h := newChatHistory(20, time.Hour)
	h.Append("telegram:1", compute.Message{Role: "user", Content: "alice"})
	h.Append("telegram:2", compute.Message{Role: "user", Content: "bob"})
	a := h.Load("telegram:1")
	b := h.Load("telegram:2")
	if len(a) != 1 || a[0].Content != "alice" {
		t.Errorf("chat 1: %+v", a)
	}
	if len(b) != 1 || b[0].Content != "bob" {
		t.Errorf("chat 2: %+v", b)
	}
}

func TestNewTurnMessagesStripsSystemAndPrior(t *testing.T) {
	t.Parallel()
	all := []compute.Message{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: "old user msg"},
		{Role: "assistant", Content: "old assistant reply"},
		{Role: "user", Content: "new user msg"},
		{Role: "assistant", ToolCalls: []compute.ToolCall{{Name: "fetch_url"}}},
		{Role: "tool", Content: "tool result"},
		{Role: "assistant", Content: "final reply"},
	}
	// turnStart is an absolute index into the list: system + 2 prior.
	got := newTurnMessages(all, 3)
	if len(got) != 4 {
		t.Fatalf("len=%d want 4 (user + asst-tool-call + tool result + final asst)", len(got))
	}
	if got[0].Role != "user" || got[0].Content != "new user msg" {
		t.Errorf("first new msg: %+v", got[0])
	}
	if len(got[1].ToolCalls) != 1 {
		t.Errorf("tool call should survive: %+v", got[1])
	}
	if got[2].Role != "tool" {
		t.Errorf("tool result should survive: %+v", got[2])
	}
	if got[3].Content != "final reply" {
		t.Errorf("final reply: %+v", got[3])
	}
}

func TestNewTurnMessagesNoSystemPrefix(t *testing.T) {
	t.Parallel()
	all := []compute.Message{
		{Role: "user", Content: "u"},
		{Role: "assistant", Content: "a"},
	}
	got := newTurnMessages(all, 0)
	if len(got) != 2 {
		t.Errorf("without system prefix all messages should appear: %+v", got)
	}
}

func TestNewTurnMessagesOverflowSafe(t *testing.T) {
	t.Parallel()
	all := []compute.Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "u"},
	}
	// Defensive: an index past the end shouldn't crash — a lost turn
	// beats a crashed gateway.
	if got := newTurnMessages(all, 100); got != nil {
		t.Errorf("overflow should yield nil; got %+v", got)
	}
	if got := newTurnMessages(all, -1); got != nil {
		t.Errorf("negative index should yield nil; got %+v", got)
	}
}

func TestChatHistoryForget(t *testing.T) {
	t.Parallel()
	h := newChatHistory(20, time.Hour)
	h.Append("telegram:1", compute.Message{Role: "user", Content: "hello"})
	h.Forget("telegram:1")
	if got := h.Load("telegram:1"); len(got) != 0 {
		t.Errorf("Forget should clear; got %d", len(got))
	}
}

// fakeSessionStore is an in-process SessionStore for exercising the
// conversationLog tiering without a raft cluster.
type fakeSessionStore struct {
	mu        sync.Mutex
	msgs      map[string][]compute.Message
	summaries map[string]string
	appendErr error
	loadErr   error
	appends   int
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{msgs: map[string][]compute.Message{}, summaries: map[string]string{}}
}

func (f *fakeSessionStore) LoadTranscript(_ context.Context, ref SessionRef, n int) (Transcript, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		return Transcript{}, f.loadErr
	}
	all := f.msgs[cacheKey(ref)]
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return Transcript{
		Summary:  f.summaries[cacheKey(ref)],
		Messages: append([]compute.Message(nil), all...),
	}, nil
}

func (f *fakeSessionStore) Append(_ context.Context, ref SessionRef, _ string, msgs []compute.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appends++
	if f.appendErr != nil {
		return f.appendErr
	}
	k := cacheKey(ref)
	f.msgs[k] = append(f.msgs[k], msgs...)
	return nil
}

func (f *fakeSessionStore) Forget(_ context.Context, ref SessionRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.msgs, cacheKey(ref))
	return nil
}

func testRef() SessionRef {
	return SessionRef{Channel: "telegram", ChannelID: "7", UserID: "alice"}
}

func TestConversationLogPersistsAndReloads(t *testing.T) {
	t.Parallel()
	store := newFakeSessionStore()
	ref := testRef()

	first := newConversationLog(store, nil, ConversationConfig{}, nil)
	first.Append(context.Background(), ref, "turn-1", []compute.Message{
		{Role: "user", Content: "remember this"},
		{Role: "assistant", Content: "noted"},
	})

	// A fresh log = a restarted process: empty cache, durable store
	// intact. This is the whole point of the feature.
	second := newConversationLog(store, nil, ConversationConfig{}, nil)
	got := second.Load(context.Background(), ref).Messages
	if len(got) != 2 {
		t.Fatalf("after restart Load = %d messages; want 2", len(got))
	}
	if got[0].Content != "remember this" {
		t.Errorf("lost content across restart: %+v", got)
	}
}

func TestConversationLogWithoutDurableStoreUsesCache(t *testing.T) {
	t.Parallel()
	c := newConversationLog(nil, nil, ConversationConfig{}, nil)
	ref := testRef()
	c.Append(context.Background(), ref, "t", []compute.Message{{Role: "user", Content: "hi"}})
	if got := c.Load(context.Background(), ref).Messages; len(got) != 1 {
		t.Errorf("Load = %d; want 1 from cache", len(got))
	}
}

// The follower case: durable writes fail, so the conversation has to
// stay coherent on the cache alone.
func TestConversationLogFallsBackWhenNotLeader(t *testing.T) {
	t.Parallel()
	store := newFakeSessionStore()
	store.appendErr = ErrSessionUnavailable
	c := newConversationLog(store, nil, ConversationConfig{}, nil)
	ref := testRef()

	c.Append(context.Background(), ref, "t", []compute.Message{{Role: "user", Content: "on a follower"}})
	got := c.Load(context.Background(), ref).Messages
	if len(got) != 1 || got[0].Content != "on a follower" {
		t.Errorf("Load = %+v; want the cached message despite the failed durable write", got)
	}
}

func TestConversationLogFallsBackWhenDurableReadFails(t *testing.T) {
	t.Parallel()
	store := newFakeSessionStore()
	c := newConversationLog(store, nil, ConversationConfig{}, nil)
	ref := testRef()
	c.Append(context.Background(), ref, "t", []compute.Message{{Role: "user", Content: "cached"}})

	store.mu.Lock()
	store.loadErr = errors.New("bbolt exploded")
	store.mu.Unlock()

	got := c.Load(context.Background(), ref).Messages
	if len(got) != 1 || got[0].Content != "cached" {
		t.Errorf("Load = %+v; want cache fallback on durable read error", got)
	}
}

func TestConversationLogForgetClearsBothTiers(t *testing.T) {
	t.Parallel()
	store := newFakeSessionStore()
	c := newConversationLog(store, nil, ConversationConfig{}, nil)
	ref := testRef()
	c.Append(context.Background(), ref, "t", []compute.Message{{Role: "user", Content: "x"}})

	if err := c.Forget(context.Background(), ref); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if got := c.Load(context.Background(), ref).Messages; len(got) != 0 {
		t.Errorf("Load after Forget = %d; want 0", len(got))
	}
	if got, _ := store.LoadTranscript(context.Background(), ref, 0); len(got.Messages) != 0 {
		t.Errorf("durable store still holds %d messages after Forget", len(got.Messages))
	}
}

func TestConversationLogSkipsEmptyAppend(t *testing.T) {
	t.Parallel()
	store := newFakeSessionStore()
	c := newConversationLog(store, nil, ConversationConfig{}, nil)
	c.Append(context.Background(), testRef(), "t", nil)
	if store.appends != 0 {
		t.Errorf("empty append reached the durable store %d times", store.appends)
	}
}

func TestConversationLogHonoursConfiguredTail(t *testing.T) {
	t.Parallel()
	store := newFakeSessionStore()
	ref := testRef()
	c := newConversationLog(store, nil, ConversationConfig{TailMessages: 3}, nil)
	for i := range 10 {
		c.Append(context.Background(), ref, "t", []compute.Message{
			{Role: "user", Content: fmt.Sprintf("m%d", i)},
		})
	}
	if got := c.Load(context.Background(), ref).Messages; len(got) != 3 {
		t.Errorf("loaded %d messages, want the configured tail of 3", len(got))
	}
}

func TestConversationCacheHonoursConfiguredSize(t *testing.T) {
	t.Parallel()
	// No durable store, so everything rides on the cache.
	c := newConversationLog(nil, nil, ConversationConfig{CacheMessages: 2, CacheTTL: time.Hour}, nil)
	ref := testRef()
	for i := range 8 {
		c.Append(context.Background(), ref, "t", []compute.Message{
			{Role: "user", Content: fmt.Sprintf("m%d", i)},
		})
	}
	if got := c.Load(context.Background(), ref).Messages; len(got) != 2 {
		t.Errorf("cache held %d messages, want the configured 2", len(got))
	}
}
