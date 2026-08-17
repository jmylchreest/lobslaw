package memory

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// `lobslaw session list` printed "Total sessions: 0" on any machine
// that is not the node, because there was no state.db to open — and a
// count of zero reads as a quiet cluster rather than as the wrong
// store.

func newSessionRPC(t *testing.T) (*SessionRPC, *SessionService) {
	t.Helper()
	svc := testSessionService(t, 0)
	return NewSessionRPC(svc), svc
}

// refID is the store key a SessionRef resolves to, so a test can ask
// for the conversation it just wrote.
func refID(t *testing.T, ref SessionRef) string {
	t.Helper()
	id, err := sessionID(ref.Channel, ref.ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func appendTurn(t *testing.T, svc *SessionService, ref SessionRef, texts ...string) {
	t.Helper()
	msgs := make([]TranscriptMessage, 0, len(texts))
	for _, txt := range texts {
		msgs = append(msgs, TranscriptMessage{Role: "user", Content: txt})
	}
	if _, err := svc.Append(context.Background(), ref, "turn-1", msgs); err != nil {
		t.Fatal(err)
	}
}

// --- listing -----------------------------------------------------------

func TestListSessionsReturnsWhatWasWritten(t *testing.T) {
	t.Parallel()
	rpc, svc := newSessionRPC(t)
	appendTurn(t, svc, SessionRef{Channel: "telegram", ChannelID: "c1", UserID: "u1"}, "hello")

	res, err := rpc.ListSessions(context.Background(), &lobslawv1.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetSessions()) != 1 {
		t.Fatalf("got %d sessions", len(res.GetSessions()))
	}
}

// A channel filter that matched nothing and a filter that was ignored
// look the same from the outside, so both directions are asserted.
func TestTheChannelFilterNarrows(t *testing.T) {
	t.Parallel()
	rpc, svc := newSessionRPC(t)
	appendTurn(t, svc, SessionRef{Channel: "telegram", ChannelID: "c1", UserID: "u1"}, "hello")
	appendTurn(t, svc, SessionRef{Channel: "rest", ChannelID: "c2", UserID: "u2"}, "hi")

	tg, err := rpc.ListSessions(context.Background(),
		&lobslawv1.ListSessionsRequest{Channel: "telegram"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tg.GetSessions()) != 1 || tg.GetSessions()[0].GetChannel() != "telegram" {
		t.Errorf("telegram filter returned %v", tg.GetSessions())
	}

	all, err := rpc.ListSessions(context.Background(), &lobslawv1.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.GetSessions()) != 2 {
		t.Errorf("an empty filter returned %d; it should not filter", len(all.GetSessions()))
	}
}

func TestTheUserFilterNarrows(t *testing.T) {
	t.Parallel()
	rpc, svc := newSessionRPC(t)
	appendTurn(t, svc, SessionRef{Channel: "telegram", ChannelID: "c1", UserID: "alice"}, "hello")
	appendTurn(t, svc, SessionRef{Channel: "telegram", ChannelID: "c2", UserID: "bob"}, "hi")

	res, err := rpc.ListSessions(context.Background(),
		&lobslawv1.ListSessionsRequest{UserId: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetSessions()) != 1 || res.GetSessions()[0].GetUserId() != "alice" {
		t.Errorf("user filter returned %v", res.GetSessions())
	}
}

// --- one transcript ----------------------------------------------------

func TestGetSessionReturnsTheTranscriptInOrder(t *testing.T) {
	t.Parallel()
	rpc, svc := newSessionRPC(t)
	ref := SessionRef{Channel: "telegram", ChannelID: "c1", UserID: "u1"}
	appendTurn(t, svc, ref, "first", "second", "third")

	res, err := rpc.GetSession(context.Background(), &lobslawv1.GetSessionRequest{Id: refID(t, ref)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetMessages()) != 3 {
		t.Fatalf("got %d messages", len(res.GetMessages()))
	}
	// Sequence order, not store order. A transcript read out of order
	// is a different conversation.
	for i := 1; i < len(res.GetMessages()); i++ {
		if res.GetMessages()[i].GetSeq() <= res.GetMessages()[i-1].GetSeq() {
			t.Fatalf("messages are out of sequence: %d then %d",
				res.GetMessages()[i-1].GetSeq(), res.GetMessages()[i].GetSeq())
		}
	}
	if res.GetSession().GetId() != refID(t, ref) {
		t.Errorf("session = %v", res.GetSession())
	}
}

// A conversation with no messages and a conversation that does not
// exist are different answers, and only one means the id was wrong.
func TestAnUnknownSessionIsNotFound(t *testing.T) {
	t.Parallel()
	rpc, _ := newSessionRPC(t)

	_, err := rpc.GetSession(context.Background(), &lobslawv1.GetSessionRequest{Id: "no-such"})
	if err == nil {
		t.Fatal("an unknown session returned a success")
	}
	if status.Code(err) != codes.NotFound {
		t.Errorf("code = %v, want NotFound", status.Code(err))
	}
}

func TestGetSessionNeedsAnId(t *testing.T) {
	t.Parallel()
	rpc, _ := newSessionRPC(t)
	if _, err := rpc.GetSession(context.Background(), &lobslawv1.GetSessionRequest{}); err == nil {
		t.Error("an empty id was accepted")
	}
}

// --- search ------------------------------------------------------------

func TestSearchFindsTheConversation(t *testing.T) {
	t.Parallel()
	rpc, svc := newSessionRPC(t)
	appendTurn(t, svc, SessionRef{Channel: "telegram", ChannelID: "c1", UserID: "u1"},
		"the pelican brief", "something else")
	appendTurn(t, svc, SessionRef{Channel: "telegram", ChannelID: "c2", UserID: "u1"},
		"nothing relevant here")

	res, err := rpc.SearchSessions(context.Background(),
		&lobslawv1.SearchSessionsRequest{Text: "pelican"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetHits()) != 1 {
		t.Fatalf("got %d hits", len(res.GetHits()))
	}
	if len(res.GetHits()[0].GetSnippets()) == 0 {
		t.Error("a hit with no snippet gives the operator nothing to judge relevance on")
	}
	if res.GetHits()[0].GetMatches() < 1 {
		t.Error("the match count is zero on a hit")
	}
}

// Enumeration is ListSessions. An empty search returning every
// conversation would read as "they all mention it".
func TestAnEmptySearchIsRefused(t *testing.T) {
	t.Parallel()
	rpc, svc := newSessionRPC(t)
	appendTurn(t, svc, SessionRef{Channel: "telegram", ChannelID: "c1", UserID: "u1"}, "hello")

	_, err := rpc.SearchSessions(context.Background(), &lobslawv1.SearchSessionsRequest{})
	if err == nil {
		t.Fatal("an empty search was accepted")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", status.Code(err))
	}
}

// The match count is the total, which may exceed the snippets shown —
// it is what tells a passing mention from a thread about the thing.
func TestTheMatchCountCanExceedTheSnippets(t *testing.T) {
	t.Parallel()
	rpc, svc := newSessionRPC(t)
	ref := SessionRef{Channel: "telegram", ChannelID: "c1", UserID: "u1"}
	appendTurn(t, svc, ref, "pelican one", "pelican two", "pelican three", "pelican four")

	res, err := rpc.SearchSessions(context.Background(), &lobslawv1.SearchSessionsRequest{
		Text: "pelican", SnippetsPerSession: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetHits()) != 1 {
		t.Fatalf("got %d hits", len(res.GetHits()))
	}
	hit := res.GetHits()[0]
	if len(hit.GetSnippets()) != 2 {
		t.Errorf("snippets = %d, want the cap of 2", len(hit.GetSnippets()))
	}
	if hit.GetMatches() <= int32(len(hit.GetSnippets())) {
		t.Errorf("matches = %d with %d snippets; the total was lost to the cap",
			hit.GetMatches(), len(hit.GetSnippets()))
	}
}

// --- no store ----------------------------------------------------------

// A service answering every call with "not wired" is worse than an
// unimplemented one: a client cannot tell it from an empty cluster.
func TestAnUnwiredServiceRefusesRatherThanAnsweringEmpty(t *testing.T) {
	t.Parallel()
	rpc := NewSessionRPC(nil)

	if _, err := rpc.ListSessions(context.Background(), &lobslawv1.ListSessionsRequest{}); err == nil {
		t.Error("an unwired service returned an empty list")
	}
	if _, err := rpc.GetSession(context.Background(),
		&lobslawv1.GetSessionRequest{Id: "x"}); err == nil {
		t.Error("an unwired service returned an empty transcript")
	}
	if _, err := rpc.SearchSessions(context.Background(),
		&lobslawv1.SearchSessionsRequest{Text: "x"}); err == nil {
		t.Error("an unwired service returned no matches")
	}
}
