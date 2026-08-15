package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func testSessionService(t *testing.T, maxMessages int) *SessionService {
	t.Helper()
	svc := newTestServiceStack(t)
	return NewSessionService(svc.raft, svc.store, SessionConfig{MaxMessages: maxMessages})
}

func userMsg(text string) TranscriptMessage {
	return TranscriptMessage{Role: "user", Content: text}
}

func assistantMsg(text string) TranscriptMessage {
	return TranscriptMessage{Role: "assistant", Content: text}
}

func TestSessionAppendLoadRoundTrip(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	ctx := context.Background()
	ref := SessionRef{Channel: "telegram", ChannelID: "12345", UserID: "alice"}

	if _, err := s.Append(ctx, ref, "turn-1", []TranscriptMessage{
		userMsg("hello"),
		assistantMsg("hi there"),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := s.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}
	if got[0].Role != "user" || got[0].Content != "hello" {
		t.Errorf("first message = %+v, want user/hello", got[0])
	}
	if got[1].Role != "assistant" || got[1].Content != "hi there" {
		t.Errorf("second message = %+v, want assistant/hi there", got[1])
	}
}

func TestSessionLoadMissingIsNotAnError(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	got, err := s.Load(context.Background(), SessionRef{Channel: "rest", ChannelID: "nobody"})
	if err != nil {
		t.Fatalf("Load on absent session: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d messages, want 0", len(got))
	}
}

func TestSessionAppendPreservesOrderAcrossTurns(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	ctx := context.Background()
	ref := SessionRef{Channel: "telegram", ChannelID: "1"}

	for i := range 5 {
		if _, err := s.Append(ctx, ref, fmt.Sprintf("turn-%d", i), []TranscriptMessage{
			userMsg(fmt.Sprintf("q%d", i)),
			assistantMsg(fmt.Sprintf("a%d", i)),
		}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	got, err := s.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("got %d messages, want 10", len(got))
	}
	for i := range 5 {
		if want := fmt.Sprintf("q%d", i); got[i*2].Content != want {
			t.Errorf("message %d = %q, want %q", i*2, got[i*2].Content, want)
		}
		if want := fmt.Sprintf("a%d", i); got[i*2+1].Content != want {
			t.Errorf("message %d = %q, want %q", i*2+1, got[i*2+1].Content, want)
		}
	}
}

// Ordering must survive the seq crossing a digit boundary — the whole
// point of zero-padding the key. Without padding, "…:10" sorts before
// "…:9" and the transcript silently scrambles after nine messages.
func TestSessionOrderingSurvivesDigitRollover(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	ctx := context.Background()
	ref := SessionRef{Channel: "rest", ChannelID: "rollover"}

	for i := range 12 {
		if _, err := s.Append(ctx, ref, "t", []TranscriptMessage{userMsg(fmt.Sprintf("%02d", i))}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	got, err := s.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 12 {
		t.Fatalf("got %d messages, want 12", len(got))
	}
	for i := range 12 {
		if want := fmt.Sprintf("%02d", i); got[i].Content != want {
			t.Errorf("position %d = %q, want %q (ordering scrambled)", i, got[i].Content, want)
		}
	}
}

func TestSessionTrimsToMaxMessages(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 4)
	ctx := context.Background()
	ref := SessionRef{Channel: "telegram", ChannelID: "trim"}

	for i := range 5 {
		if _, err := s.Append(ctx, ref, "t", []TranscriptMessage{
			userMsg(fmt.Sprintf("q%d", i)),
			assistantMsg(fmt.Sprintf("a%d", i)),
		}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	got, err := s.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d messages, want 4 (the cap)", len(got))
	}
	// Oldest dropped, newest kept.
	want := []string{"q3", "a3", "q4", "a4"}
	for i, w := range want {
		if got[i].Content != w {
			t.Errorf("position %d = %q, want %q", i, got[i].Content, w)
		}
	}
}

// Trimming must actually delete the evicted records, not just move
// the FirstSeq cursor past them — otherwise a busy chat leaks rows
// into the shared bucket forever.
func TestSessionTrimDeletesEvictedRecords(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	s := NewSessionService(svc.raft, svc.store, SessionConfig{MaxMessages: 2})
	ctx := context.Background()
	ref := SessionRef{Channel: "telegram", ChannelID: "leak"}

	for i := range 6 {
		if _, err := s.Append(ctx, ref, "t", []TranscriptMessage{userMsg(fmt.Sprintf("m%d", i))}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	var stored int
	err := svc.store.ForEachPrefix(BucketSessionMessages, "telegram:leak:", func(string, []byte) error {
		stored++
		return nil
	})
	if err != nil {
		t.Fatalf("ForEachPrefix: %v", err)
	}
	if stored != 2 {
		t.Errorf("%d message records on disk, want 2 — evicted records leaked", stored)
	}
}

func TestSessionToolCallsRoundTrip(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	ctx := context.Background()
	ref := SessionRef{Channel: "rest", ChannelID: "tools"}

	if _, err := s.Append(ctx, ref, "turn-1", []TranscriptMessage{
		userMsg("what's in /tmp?"),
		{
			Role: "assistant",
			ToolCalls: []TranscriptToolCall{
				{ID: "call_1", Name: "list_files", Arguments: `{"path":"/tmp"}`},
			},
		},
		{Role: "tool", ToolCallID: "call_1", Content: "a.txt\nb.txt"},
		assistantMsg("two files."),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := s.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d messages, want 4", len(got))
	}
	if len(got[1].ToolCalls) != 1 {
		t.Fatalf("assistant message lost its tool calls: %+v", got[1])
	}
	tc := got[1].ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "list_files" || tc.Arguments != `{"path":"/tmp"}` {
		t.Errorf("tool call = %+v, want call_1/list_files with args", tc)
	}
	if got[2].Role != "tool" || got[2].ToolCallID != "call_1" {
		t.Errorf("tool result = %+v, want role=tool linked to call_1", got[2])
	}
}

// The system prompt is rebuilt per turn by promptgen from live SOUL +
// tool state; persisting a copy would replay a stale identity.
func TestSessionDropsSystemMessages(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	ctx := context.Background()
	ref := SessionRef{Channel: "rest", ChannelID: "sys"}

	if _, err := s.Append(ctx, ref, "t", []TranscriptMessage{
		{Role: "system", Content: "you are lobslaw"},
		userMsg("hi"),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := s.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1 (system dropped)", len(got))
	}
	if got[0].Role != "user" {
		t.Errorf("retained %q, want the user message", got[0].Role)
	}
}

func TestSessionLoadTailLimits(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	ctx := context.Background()
	ref := SessionRef{Channel: "rest", ChannelID: "tail"}

	for i := range 10 {
		if _, err := s.Append(ctx, ref, "t", []TranscriptMessage{userMsg(fmt.Sprintf("m%d", i))}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	got, err := s.LoadTail(ctx, ref, 3)
	if err != nil {
		t.Fatalf("LoadTail: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3", len(got))
	}
	if got[0].Content != "m7" || got[2].Content != "m9" {
		t.Errorf("tail = %q..%q, want m7..m9", got[0].Content, got[2].Content)
	}
}

func TestSessionForgetPurgesTranscript(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	s := NewSessionService(svc.raft, svc.store, SessionConfig{})
	ctx := context.Background()
	ref := SessionRef{Channel: "telegram", ChannelID: "gone"}

	for i := range 3 {
		if _, err := s.Append(ctx, ref, "t", []TranscriptMessage{userMsg(fmt.Sprintf("m%d", i))}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := s.Forget(ctx, ref); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	got, err := s.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load after Forget: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d messages after Forget, want 0", len(got))
	}

	var leftover int
	if err := svc.store.ForEachPrefix(BucketSessionMessages, "telegram:gone:", func(string, []byte) error {
		leftover++
		return nil
	}); err != nil {
		t.Fatalf("ForEachPrefix: %v", err)
	}
	if leftover != 0 {
		t.Errorf("%d orphaned message records after Forget, want 0", leftover)
	}
}

// Sessions share one bucket, so a prefix that isn't ':'-terminated
// would let "rest:1" read "rest:10"'s transcript.
func TestSessionsDoNotBleedAcrossSimilarIDs(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	ctx := context.Background()
	short := SessionRef{Channel: "rest", ChannelID: "1"}
	long := SessionRef{Channel: "rest", ChannelID: "10"}

	if _, err := s.Append(ctx, short, "t", []TranscriptMessage{userMsg("i am one")}); err != nil {
		t.Fatalf("Append short: %v", err)
	}
	if _, err := s.Append(ctx, long, "t", []TranscriptMessage{userMsg("i am ten")}); err != nil {
		t.Fatalf("Append long: %v", err)
	}

	got, err := s.Load(ctx, short)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].Content != "i am one" {
		t.Errorf("session rest:1 = %+v, want just its own message", got)
	}
}

func TestSessionRejectsColonInComponents(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	ctx := context.Background()
	_, err := s.Append(ctx, SessionRef{Channel: "tele:gram", ChannelID: "1"}, "t",
		[]TranscriptMessage{userMsg("x")})
	if err == nil || !strings.Contains(err.Error(), "':'") {
		t.Errorf("expected colon rejection, got %v", err)
	}
}

func TestSessionListReturnsIndexRecords(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	ctx := context.Background()

	for _, id := range []string{"a", "b"} {
		ref := SessionRef{Channel: "telegram", ChannelID: id, UserID: "alice"}
		if _, err := s.Append(ctx, ref, "t", []TranscriptMessage{userMsg("hi")}); err != nil {
			t.Fatalf("Append %s: %v", id, err)
		}
	}
	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2", len(got))
	}
	for _, rec := range got {
		if rec.UserId != "alice" {
			t.Errorf("session %s user = %q, want alice", rec.Id, rec.UserId)
		}
		if rec.Channel != "telegram" {
			t.Errorf("session %s channel = %q, want telegram", rec.Id, rec.Channel)
		}
	}
}

// A node with no raft at all cannot write and cannot forward either.
// Distinct from a follower, which now forwards to the leader — see
// TestFollowerWriteReachesLeader.
func TestSessionAppendWithoutRaftReturnsErrNotLeader(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	// No raft wired = can't be the leader; the gateway relies on
	// errors.Is here to decide whether to degrade to its cache.
	s := NewSessionService(nil, svc.store, SessionConfig{})
	_, err := s.Append(context.Background(),
		SessionRef{Channel: "rest", ChannelID: "1"}, "t",
		[]TranscriptMessage{userMsg("x")})
	if !errors.Is(err, ErrNotLeader) {
		t.Errorf("got %v, want ErrNotLeader", err)
	}
}

func TestSessionSummaryRoundTrip(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	ctx := context.Background()
	ref := SessionRef{Channel: "rest", ChannelID: "sum"}

	for i := range 6 {
		if _, err := s.Append(ctx, ref, "t", []TranscriptMessage{userMsg(fmt.Sprintf("m%d", i))}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := s.PutSummary(ctx, ref, "the user counted to five", 4); err != nil {
		t.Fatalf("PutSummary: %v", err)
	}

	tr, err := s.LoadTranscript(ctx, ref)
	if err != nil {
		t.Fatalf("LoadTranscript: %v", err)
	}
	if tr.Summary != "the user counted to five" {
		t.Errorf("summary = %q", tr.Summary)
	}
	if tr.SummaryThroughSeq != 4 {
		t.Errorf("through = %d, want 4", tr.SummaryThroughSeq)
	}
	// Summarised messages must NOT also be replayed — paying twice for
	// the same content, and inviting the model to treat one as a
	// correction of the other.
	if len(tr.Messages) != 2 {
		t.Fatalf("got %d verbatim messages, want 2 (seq 5 and 6): %+v", len(tr.Messages), tr.Messages)
	}
	if tr.Messages[0].Content != "m4" || tr.Messages[1].Content != "m5" {
		t.Errorf("wrong tail: %+v", tr.Messages)
	}
}

// A stale compaction landing after a newer one would resurrect
// messages the newer summary already folded in.
func TestSessionSummaryNeverGoesBackwards(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	ctx := context.Background()
	ref := SessionRef{Channel: "rest", ChannelID: "monotonic"}
	for range 10 {
		if _, err := s.Append(ctx, ref, "t", []TranscriptMessage{userMsg("x")}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutSummary(ctx, ref, "newer", 8); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSummary(ctx, ref, "stale", 3); err != nil {
		t.Fatalf("stale write should be ignored, not error: %v", err)
	}
	tr, err := s.LoadTranscript(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Summary != "newer" || tr.SummaryThroughSeq != 8 {
		t.Errorf("stale compaction overwrote a newer one: %q through %d", tr.Summary, tr.SummaryThroughSeq)
	}
}

func TestSessionAppendPreservesSummary(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	ctx := context.Background()
	ref := SessionRef{Channel: "rest", ChannelID: "preserve"}
	for range 5 {
		if _, err := s.Append(ctx, ref, "t", []TranscriptMessage{userMsg("x")}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutSummary(ctx, ref, "keep me", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, ref, "t2", []TranscriptMessage{userMsg("after")}); err != nil {
		t.Fatal(err)
	}
	tr, err := s.LoadTranscript(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Summary != "keep me" || tr.SummaryThroughSeq != 3 {
		t.Errorf("append clobbered the summary: %q through %d", tr.Summary, tr.SummaryThroughSeq)
	}
}

func TestSessionLoadRangeIsExclusiveOfAfter(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	ctx := context.Background()
	ref := SessionRef{Channel: "rest", ChannelID: "rng"}
	for i := range 10 {
		if _, err := s.Append(ctx, ref, "t", []TranscriptMessage{userMsg(fmt.Sprintf("m%d", i))}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LoadRange(ctx, ref, 3, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3 (seq 4,5,6): %+v", len(got), got)
	}
	if got[0].Content != "m3" || got[2].Content != "m5" {
		t.Errorf("range = %+v; want m3..m5", got)
	}
}

func seedSession(t *testing.T, s *SessionService, ref SessionRef, texts ...string) {
	t.Helper()
	for _, txt := range texts {
		if _, err := s.Append(context.Background(), ref, "t", []TranscriptMessage{userMsg(txt)}); err != nil {
			t.Fatalf("seed %q: %v", txt, err)
		}
	}
}

func TestSearchTranscriptsFindsLiteralText(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	ctx := context.Background()
	a := SessionRef{Channel: "rest", ChannelID: "a"}
	b := SessionRef{Channel: "rest", ChannelID: "b"}
	seedSession(t, s, a, "the deploy failed with ERR_CONN_REFUSED", "trying again")
	seedSession(t, s, b, "lunch plans")

	hits, err := s.SearchTranscripts(ctx, SessionSearchQuery{Text: "err_conn_refused"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1: %+v", len(hits), hits)
	}
	if hits[0].Session.Id != "rest:a" {
		t.Errorf("matched %s", hits[0].Session.Id)
	}
	if len(hits[0].Snippets) != 1 || !strings.Contains(hits[0].Snippets[0].Text, "ERR_CONN_REFUSED") {
		t.Errorf("snippet missing the match: %+v", hits[0].Snippets)
	}
}

func TestSearchTranscriptsIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	seedSession(t, s, SessionRef{Channel: "rest", ChannelID: "c"}, "Kubernetes Ingress")
	hits, err := s.SearchTranscripts(context.Background(), SessionSearchQuery{Text: "kubernetes ingress"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Errorf("case-insensitive search found %d hits", len(hits))
	}
}

func TestSearchTranscriptsRequiresText(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	if _, err := s.SearchTranscripts(context.Background(), SessionSearchQuery{Text: "  "}); err == nil {
		t.Error("empty search should be rejected, not return everything")
	}
}

func TestSearchTranscriptsRespectsLimits(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	for i := range 6 {
		seedSession(t, s, SessionRef{Channel: "rest", ChannelID: fmt.Sprintf("s%d", i)}, "shared needle")
	}
	hits, err := s.SearchTranscripts(context.Background(), SessionSearchQuery{Text: "needle", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Errorf("got %d hits, want the limit of 2", len(hits))
	}
}

func TestSearchTranscriptsCountsAllMatchesButCapsSnippets(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	ref := SessionRef{Channel: "rest", ChannelID: "many"}
	for range 8 {
		seedSession(t, s, ref, "repeated needle here")
	}
	hits, err := s.SearchTranscripts(context.Background(), SessionSearchQuery{Text: "needle", SnippetsPerSession: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits", len(hits))
	}
	if hits[0].Matches != 8 {
		t.Errorf("Matches = %d, want all 8", hits[0].Matches)
	}
	if len(hits[0].Snippets) != 2 {
		t.Errorf("got %d snippets, want the cap of 2", len(hits[0].Snippets))
	}
}

func TestSearchTranscriptsFiltersByChannel(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	seedSession(t, s, SessionRef{Channel: "rest", ChannelID: "1"}, "shared word")
	seedSession(t, s, SessionRef{Channel: "telegram", ChannelID: "2"}, "shared word")
	hits, err := s.SearchTranscripts(context.Background(), SessionSearchQuery{Text: "shared", Channel: "telegram"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Session.Channel != "telegram" {
		t.Errorf("channel filter ignored: %+v", hits)
	}
}

// A long message must not drag its whole body into the result.
func TestSearchSnippetIsBounded(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	huge := strings.Repeat("padding ", 500) + "NEEDLE" + strings.Repeat(" padding", 500)
	seedSession(t, s, SessionRef{Channel: "rest", ChannelID: "big"}, huge)
	hits, err := s.SearchTranscripts(context.Background(), SessionSearchQuery{Text: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	snip := hits[0].Snippets[0].Text
	if len(snip) > snippetContextBytes*2 {
		t.Errorf("snippet is %d bytes, want ~%d", len(snip), snippetContextBytes)
	}
	if !strings.Contains(snip, "NEEDLE") {
		t.Error("snippet lost the match it was centred on")
	}
}

func TestPutTitleRoundTrip(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	ctx := context.Background()
	ref := SessionRef{Channel: "rest", ChannelID: "titled"}
	seedSession(t, s, ref, "hello")
	if err := s.PutTitle(ctx, ref, "  A named thread  "); err != nil {
		t.Fatal(err)
	}
	tr, err := s.LoadTranscript(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Title != "A named thread" {
		t.Errorf("title = %q (should be trimmed)", tr.Title)
	}
}

func TestPutTitlePreservesSummary(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	ctx := context.Background()
	ref := SessionRef{Channel: "rest", ChannelID: "both"}
	for range 6 {
		seedSession(t, s, ref, "x")
	}
	if err := s.PutSummary(ctx, ref, "the summary", 3); err != nil {
		t.Fatal(err)
	}
	if err := s.PutTitle(ctx, ref, "the title"); err != nil {
		t.Fatal(err)
	}
	tr, err := s.LoadTranscript(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Summary != "the summary" || tr.SummaryThroughSeq != 3 {
		t.Errorf("titling clobbered the summary: %q through %d", tr.Summary, tr.SummaryThroughSeq)
	}
	if tr.Title != "the title" {
		t.Errorf("title = %q", tr.Title)
	}
}

// --- snippet windowing -------------------------------------------------

// Search snippets go straight into the agent's context, so a window
// that cuts a character in half is corruption the model reads back as
// conversation.
func TestSnippetAroundKeepsValidUTF8(t *testing.T) {
	t.Parallel()
	// The window is snippetContextBytes/2 either side of the match, so
	// whether it lands mid-character depends on where the match sits
	// relative to the 3-byte runes around it. The padding goes BETWEEN
	// the match and the surrounding text: padding the front instead
	// would shift the match and the rune grid together and cancel out,
	// leaving a fixture that passes by alignment alone.
	for pad := range 3 {
		gap := strings.Repeat("x", pad)
		content := strings.Repeat("日本語のテキスト", 60) +
			gap + "ERROR" + gap + strings.Repeat("さらに文章", 60)
		idx := strings.Index(content, "ERROR")
		got := snippetAround(content, idx, len("ERROR"))
		if !utf8.ValidString(got) {
			t.Errorf("pad=%d: snippet is not valid UTF-8: %q", pad, got)
		}
		if strings.ContainsRune(got, utf8.RuneError) {
			t.Errorf("pad=%d: snippet contains U+FFFD: %q", pad, got)
		}
		if !strings.Contains(got, "ERROR") {
			t.Errorf("pad=%d: snippet lost the match it was centred on: %q", pad, got)
		}
	}
}

// The match offset comes from a lowercased copy of the content, so it
// is not guaranteed to address the same byte in the original. Whatever
// it points at, the window must not panic on a slice bound.
func TestSnippetAroundToleratesOutOfRangeOffset(t *testing.T) {
	t.Parallel()
	content := "短いテキスト"
	for _, idx := range []int{-5, 0, len(content), len(content) + 50} {
		got := snippetAround(content, idx, 3)
		if !utf8.ValidString(got) {
			t.Errorf("idx=%d: snippet is not valid UTF-8: %q", idx, got)
		}
	}
}

// A real search over non-ASCII content, end to end through the store.
func TestSearchTranscriptsNonASCIISnippets(t *testing.T) {
	t.Parallel()
	svc := testSessionService(t, 100)
	ref := SessionRef{Channel: "rest", ChannelID: "jp", UserID: "u1"}
	if _, err := svc.Append(context.Background(), ref, "t1", []TranscriptMessage{
		{Role: "user", Content: strings.Repeat("これはテストです。", 40) + "ERR_CONN_REFUSED" + strings.Repeat("続きの文章です。", 40)},
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := svc.SearchTranscripts(context.Background(), SessionSearchQuery{Text: "err_conn_refused"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || len(hits[0].Snippets) != 1 {
		t.Fatalf("got %d hits, want 1 with a snippet", len(hits))
	}
	snip := hits[0].Snippets[0].Text
	if !utf8.ValidString(snip) {
		t.Errorf("snippet is not valid UTF-8: %q", snip)
	}
	if !strings.Contains(snip, "ERR_CONN_REFUSED") {
		t.Errorf("snippet lost the match: %q", snip)
	}
}

// Visibility has to be applied while the full candidate set is still
// in hand. Filter after the limit instead and a busy shared node
// silently loses the caller's own results to newer threads they were
// never allowed to see.
func TestSessionSearchAppliesVisibilityBeforeLimit(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	ctx := context.Background()

	mine := SessionRef{Channel: "telegram", ChannelID: "1", UserID: "alice"}
	if _, err := s.Append(ctx, mine, "t1", []TranscriptMessage{userMsg("the deposit is 25000")}); err != nil {
		t.Fatal(err)
	}
	// Three newer conversations belonging to someone else, each of
	// which also matches: more than the limit on their own.
	for i := range 3 {
		ref := SessionRef{Channel: "telegram", ChannelID: fmt.Sprintf("9%d", i), UserID: "bob"}
		if _, err := s.Append(ctx, ref, "t1", []TranscriptMessage{userMsg("the settlement figure")}); err != nil {
			t.Fatal(err)
		}
	}

	hits, err := s.SearchTranscripts(ctx, SessionSearchQuery{
		Text:    "the",
		Limit:   2,
		Visible: func(r *lobslawv1.SessionRecord) bool { return r.UserId == "alice" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want alice's one", len(hits))
	}
	if hits[0].Session.UserId != "alice" {
		t.Errorf("hit belongs to %q", hits[0].Session.UserId)
	}
}

func TestSessionDescribeReportsOwner(t *testing.T) {
	t.Parallel()
	s := testSessionService(t, 0)
	ctx := context.Background()
	ref := SessionRef{Channel: "telegram", ChannelID: "-300", UserID: "alice"}
	if _, err := s.Append(ctx, ref, "t1", []TranscriptMessage{userMsg("hi")}); err != nil {
		t.Fatal(err)
	}

	rec, err := s.Describe(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || rec.UserId != "alice" {
		t.Fatalf("got %+v, want alice's record", rec)
	}

	// An address nobody has ever used is not an error — the caller
	// asking "who owns this?" needs to tell absent from denied.
	absent, err := s.Describe(ctx, SessionRef{Channel: "telegram", ChannelID: "404"})
	if err != nil {
		t.Fatalf("Describe on absent session: %v", err)
	}
	if absent != nil {
		t.Errorf("got %+v, want nil", absent)
	}
}
