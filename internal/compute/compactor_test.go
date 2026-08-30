package compute

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/jmylchreest/lobslaw/internal/turn"
)

type fakeSummaryStore struct {
	mu       sync.Mutex
	summary  string
	title    string
	through  uint64
	messages map[uint64]Message
	nextSeq  uint64
	puts     int
	putErr   error
}

func newFakeSummaryStore() *fakeSummaryStore {
	return &fakeSummaryStore{messages: map[uint64]Message{}, nextSeq: 1}
}

func (f *fakeSummaryStore) add(msgs ...Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range msgs {
		f.messages[f.nextSeq] = m
		f.nextSeq++
	}
}

func (f *fakeSummaryStore) Pending(context.Context, turn.SessionKey) (string, uint64, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.summary, f.through, f.nextSeq, nil
}

func (f *fakeSummaryStore) Range(_ context.Context, _ turn.SessionKey, after, through uint64) ([]Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Message
	for seq := after + 1; seq <= through; seq++ {
		if m, ok := f.messages[seq]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeSummaryStore) Title(context.Context, turn.SessionKey) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.title, nil
}

func (f *fakeSummaryStore) PutTitle(_ context.Context, _ turn.SessionKey, title string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.title = title
	return nil
}

func (f *fakeSummaryStore) PutSummary(_ context.Context, _ turn.SessionKey, summary string, through uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	f.puts++
	f.summary = summary
	f.through = through
	return nil
}

type fakeSummarizer struct {
	mu     sync.Mutex
	calls  int
	priors []string
	batch  [][]Message
	out    string
	err    error
}

func (f *fakeSummarizer) SummarizeConversation(_ context.Context, prior string, msgs []Message) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.priors = append(f.priors, prior)
	f.batch = append(f.batch, msgs)
	if f.err != nil {
		return "", f.err
	}
	if f.out != "" {
		return f.out, nil
	}
	return fmt.Sprintf("summary of %d messages", len(msgs)), nil
}

func fatMessage(text string) Message {
	// ~250 estimated tokens, so a handful crosses the trigger.
	return Message{Role: "user", Content: text + strings.Repeat(" filler", 160)}
}

func testCompactor(store SessionSummaryStore, sum ConversationSummarizer, keep, trigger int) *Compactor {
	return NewCompactor(store, sum, CompactorConfig{KeepMessages: keep, TriggerTokens: trigger})
}

func TestCompactorNoopWhenNothingHasAgedOut(t *testing.T) {
	t.Parallel()
	store := newFakeSummaryStore()
	store.add(fatMessage("a"), fatMessage("b"))
	sum := &fakeSummarizer{}
	c := testCompactor(store, sum, 40, 100)

	ran, err := c.MaybeCompact(context.Background(), turn.SessionKey{Channel: "rest", ChannelID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if ran || sum.calls != 0 {
		t.Errorf("compacted a 2-message conversation (ran=%v calls=%d)", ran, sum.calls)
	}
}

// Aged-out content below the trigger stays pending rather than paying
// for an LLM round-trip to fold in a couple of messages.
func TestCompactorWaitsForTriggerThreshold(t *testing.T) {
	t.Parallel()
	store := newFakeSummaryStore()
	for range 12 {
		store.add(Message{Role: "user", Content: "tiny"})
	}
	sum := &fakeSummarizer{}
	c := testCompactor(store, sum, 2, 100_000)

	ran, err := c.MaybeCompact(context.Background(), turn.SessionKey{})
	if err != nil {
		t.Fatal(err)
	}
	if ran || sum.calls != 0 {
		t.Errorf("summarised below the trigger (ran=%v calls=%d)", ran, sum.calls)
	}
}

func TestCompactorFoldsAgedOutMessages(t *testing.T) {
	t.Parallel()
	store := newFakeSummaryStore()
	for i := range 20 {
		store.add(fatMessage(fmt.Sprintf("m%d", i)))
	}
	sum := &fakeSummarizer{out: "the user discussed things"}
	c := testCompactor(store, sum, 5, 500)

	ran, err := c.MaybeCompact(context.Background(), turn.SessionKey{})
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("expected a compaction")
	}
	if sum.calls != 1 {
		t.Fatalf("summariser called %d times, want 1", sum.calls)
	}
	if store.summary != "the user discussed things" {
		t.Errorf("summary = %q", store.summary)
	}
	// 20 messages, keep 5 → folds through seq 15, leaving 16..20.
	if store.through != 15 {
		t.Errorf("through = %d, want 15", store.through)
	}
	if remaining := store.nextSeq - 1 - store.through; remaining != 5 {
		t.Errorf("%d messages left verbatim, want the 5 kept", remaining)
	}
}

// The recent exchange must survive verbatim even right after a
// compaction — otherwise the model loses the thread it's mid-way
// through.
func TestCompactorNeverFoldsTheKeepWindow(t *testing.T) {
	t.Parallel()
	store := newFakeSummaryStore()
	for i := range 30 {
		store.add(fatMessage(fmt.Sprintf("m%d", i)))
	}
	sum := &fakeSummarizer{}
	keep := 10
	c := testCompactor(store, sum, keep, 100)

	if _, err := c.MaybeCompact(context.Background(), turn.SessionKey{}); err != nil {
		t.Fatal(err)
	}
	remaining := store.nextSeq - 1 - store.through
	if remaining < uint64(keep) {
		t.Errorf("only %d messages left verbatim, want at least %d", remaining, keep)
	}
}

// Each message should be summarised roughly once. If compaction
// re-read the whole transcript every time, cost would grow with
// conversation length — the thing it exists to prevent.
func TestCompactionIsIncremental(t *testing.T) {
	t.Parallel()
	store := newFakeSummaryStore()
	sum := &fakeSummarizer{}
	c := testCompactor(store, sum, 4, 400)
	ctx := context.Background()

	msgNo := 0
	for range 5 {
		for range 6 {
			msgNo++
			store.add(fatMessage(fmt.Sprintf("msg-%03d", msgNo)))
		}
		if _, err := c.MaybeCompact(ctx, turn.SessionKey{}); err != nil {
			t.Fatal(err)
		}
	}

	if sum.calls < 2 {
		t.Fatalf("only %d compactions across 30 messages", sum.calls)
	}
	// No message may appear in two different batches.
	seen := map[string]int{}
	for _, batch := range sum.batch {
		for _, m := range batch {
			seen[m.Content]++
		}
	}
	for content, n := range seen {
		if n > 1 {
			t.Errorf("message %q was summarised %d times — compaction is not incremental",
				content[:9], n)
		}
	}
	// And each call after the first must carry the running summary
	// forward rather than starting fresh.
	for i, prior := range sum.priors[1:] {
		if prior == "" {
			t.Errorf("compaction %d discarded the prior summary", i+1)
		}
	}
}

func TestCompactorPropagatesSummarizerFailure(t *testing.T) {
	t.Parallel()
	store := newFakeSummaryStore()
	for range 20 {
		store.add(fatMessage("x"))
	}
	boom := errors.New("provider exploded")
	c := testCompactor(store, &fakeSummarizer{err: boom}, 5, 100)

	ran, err := c.MaybeCompact(context.Background(), turn.SessionKey{})
	if ran || !errors.Is(err, boom) {
		t.Errorf("ran=%v err=%v; want failure surfaced", ran, err)
	}
	if store.puts != 0 {
		t.Error("stored a summary despite the summariser failing")
	}
}

func TestCompactorRejectsEmptySummary(t *testing.T) {
	t.Parallel()
	store := newFakeSummaryStore()
	for range 20 {
		store.add(fatMessage("x"))
	}
	c := testCompactor(store, &fakeSummarizer{out: "   "}, 5, 100)
	if _, err := c.MaybeCompact(context.Background(), turn.SessionKey{}); err == nil {
		t.Error("empty summary should be an error, not stored")
	}
	if store.puts != 0 {
		t.Error("stored an empty summary")
	}
}

// The summary rides on every subsequent turn, so a model that ignores
// the length instruction can't be allowed to set the cost.
func TestCompactorCapsSummaryLength(t *testing.T) {
	t.Parallel()
	store := newFakeSummaryStore()
	for range 20 {
		store.add(fatMessage("x"))
	}
	huge := strings.Repeat("this summary is far too long. ", 500)
	c := NewCompactor(store, &fakeSummarizer{out: huge},
		CompactorConfig{KeepMessages: 5, TriggerTokens: 100, MaxSummaryTokens: 100})

	if _, err := c.MaybeCompact(context.Background(), turn.SessionKey{}); err != nil {
		t.Fatal(err)
	}
	if len(store.summary) > 100*4+8 {
		t.Errorf("summary is %d bytes, cap was ~400", len(store.summary))
	}
	if !strings.HasSuffix(strings.TrimSpace(store.summary), ".") {
		t.Errorf("summary should be cut at a sentence boundary: %q", store.summary[len(store.summary)-40:])
	}
}

func TestNewCompactorNilWithoutDependencies(t *testing.T) {
	t.Parallel()
	if c := NewCompactor(nil, &fakeSummarizer{}, CompactorConfig{}); c != nil {
		t.Error("no store should mean no compactor")
	}
	if c := NewCompactor(newFakeSummaryStore(), nil, CompactorConfig{}); c != nil {
		t.Error("no summariser should mean no compactor")
	}
}

// A nil compactor is the "feature off" signal, so calling it must be
// safe rather than a nil-pointer panic at the call site.
func TestNilCompactorIsSafeToCall(t *testing.T) {
	t.Parallel()
	var c *Compactor
	ran, err := c.MaybeCompact(context.Background(), turn.SessionKey{})
	if ran || err != nil {
		t.Errorf("ran=%v err=%v; want a silent no-op", ran, err)
	}
}

type fakeTitler struct {
	mu    sync.Mutex
	calls int
	out   string
	err   error
}

func (f *fakeTitler) Title(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	if f.out != "" {
		return f.out, nil
	}
	return "a conversation", nil
}

func TestCompactorTitlesOnFirstCompaction(t *testing.T) {
	t.Parallel()
	store := newFakeSummaryStore()
	for range 20 {
		store.add(fatMessage("x"))
	}
	titler := &fakeTitler{out: "raft snapshot corruption"}
	c := NewCompactor(store, &fakeSummarizer{},
		CompactorConfig{KeepMessages: 5, TriggerTokens: 100, Titler: titler})

	if _, err := c.MaybeCompact(context.Background(), turn.SessionKey{}); err != nil {
		t.Fatal(err)
	}
	if store.title != "raft snapshot corruption" {
		t.Errorf("title = %q", store.title)
	}
}

// Re-titling on every compaction would spend a call per compaction to
// churn a label nobody asked to change.
func TestCompactorTitlesOnlyOnce(t *testing.T) {
	t.Parallel()
	store := newFakeSummaryStore()
	titler := &fakeTitler{}
	c := NewCompactor(store, &fakeSummarizer{},
		CompactorConfig{KeepMessages: 4, TriggerTokens: 200, Titler: titler})
	ctx := context.Background()

	for range 5 {
		for range 6 {
			store.add(fatMessage("x"))
		}
		if _, err := c.MaybeCompact(ctx, turn.SessionKey{}); err != nil {
			t.Fatal(err)
		}
	}
	if titler.calls != 1 {
		t.Errorf("titler called %d times across 5 compactions, want 1", titler.calls)
	}
}

// An untitled conversation is a cosmetic loss; the compaction that
// preceded it already succeeded and must not be reported as failed.
func TestCompactorSurvivesTitlerFailure(t *testing.T) {
	t.Parallel()
	store := newFakeSummaryStore()
	for range 20 {
		store.add(fatMessage("x"))
	}
	c := NewCompactor(store, &fakeSummarizer{},
		CompactorConfig{KeepMessages: 5, TriggerTokens: 100,
			Titler: &fakeTitler{err: errors.New("model down")}})

	ran, err := c.MaybeCompact(context.Background(), turn.SessionKey{})
	if !ran || err != nil {
		t.Errorf("ran=%v err=%v; a titling failure must not fail the compaction", ran, err)
	}
	if store.summary == "" {
		t.Error("summary was not stored")
	}
}

func TestCompactorWithoutTitlerLeavesSessionsUntitled(t *testing.T) {
	t.Parallel()
	store := newFakeSummaryStore()
	for range 20 {
		store.add(fatMessage("x"))
	}
	c := testCompactor(store, &fakeSummarizer{}, 5, 100)
	if _, err := c.MaybeCompact(context.Background(), turn.SessionKey{}); err != nil {
		t.Fatal(err)
	}
	if store.title != "" {
		t.Errorf("title = %q; want none without a titler", store.title)
	}
}

// The stored summary is prepended to every later turn, so a character
// broken by the token ceiling is charged repeatedly — and the summary
// is the one piece of context deliberately kept when everything else
// is trimmed away.
func TestTruncateToTokensKeepsValidUTF8(t *testing.T) {
	t.Parallel()
	for _, maxTokens := range []int{1, 2, 3, 25} {
		got := truncateToTokens(strings.Repeat("日", 200), maxTokens)
		if !utf8.ValidString(got) {
			t.Errorf("maxTokens=%d: summary is not valid UTF-8: %q", maxTokens, got)
		}
		if strings.ContainsRune(got, utf8.RuneError) {
			t.Errorf("maxTokens=%d: summary contains U+FFFD: %q", maxTokens, got)
		}
	}
}
