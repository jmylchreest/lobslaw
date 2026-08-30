package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/auth"
)

// startRESTWithSessions is startREST plus a durable session store, so
// a test can assert what survives across server instances.
func startRESTWithSessions(t *testing.T, agent *compute.Agent, sessions SessionStore) string {
	t.Helper()
	srv := NewServer(RESTConfig{Addr: "127.0.0.1:0", Sessions: sessions}, agent)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = srv.Start(ctx)
	})
	deadline := time.Now().Add(time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.Addr() == "" {
		cancel()
		wg.Wait()
		t.Fatal("server didn't bind within 1s")
	}
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return "http://" + srv.Addr()
}

func postMessage(t *testing.T, url string, body messageRequest) messageResponse {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url+"/v1/messages", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/messages: status %d", resp.StatusCode)
	}
	var out messageResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A second request carrying the same session_id must arrive at the
// model with the first exchange already in the message list.
func TestRESTSessionIDCarriesHistoryIntoNextTurn(t *testing.T) {
	t.Parallel()
	provider := compute.NewMockProvider(
		compute.MockResponse{Content: "first reply"},
		compute.MockResponse{Content: "second reply"},
	)
	agent, err := compute.NewAgent(compute.AgentConfig{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	url := startRESTWithSessions(t, agent, newFakeSessionStore())

	postMessage(t, url, messageRequest{Message: "my name is james", TurnID: "t1", SessionID: "s1"})
	postMessage(t, url, messageRequest{Message: "what is my name?", TurnID: "t2", SessionID: "s1"})

	calls := provider.Calls()
	if len(calls) != 2 {
		t.Fatalf("provider saw %d calls, want 2", len(calls))
	}
	var got []string
	for _, m := range calls[1].Messages {
		got = append(got, m.Role+":"+m.Content)
	}
	joined := strings.Join(got, " | ")
	if !strings.Contains(joined, "my name is james") {
		t.Errorf("second turn lost the first user message; messages were: %s", joined)
	}
	if !strings.Contains(joined, "first reply") {
		t.Errorf("second turn lost the first assistant reply; messages were: %s", joined)
	}
}

// Without a session_id the endpoint stays stateless — a shared token
// firing independent one-shot requests must not accumulate a thread.
func TestRESTWithoutSessionIDStaysStateless(t *testing.T) {
	t.Parallel()
	provider := compute.NewMockProvider(
		compute.MockResponse{Content: "first reply"},
		compute.MockResponse{Content: "second reply"},
	)
	agent, err := compute.NewAgent(compute.AgentConfig{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	url := startRESTWithSessions(t, agent, newFakeSessionStore())

	postMessage(t, url, messageRequest{Message: "one", TurnID: "t1"})
	postMessage(t, url, messageRequest{Message: "two", TurnID: "t2"})

	calls := provider.Calls()
	if len(calls) != 2 {
		t.Fatalf("provider saw %d calls, want 2", len(calls))
	}
	for _, m := range calls[1].Messages {
		if strings.Contains(m.Content, "one") {
			t.Errorf("stateless request leaked prior turn: %+v", calls[1].Messages)
		}
	}
}

// The point of the durable store: a brand-new server process picks up
// the conversation where the previous one left off.
func TestRESTSessionSurvivesServerRestart(t *testing.T) {
	t.Parallel()
	store := newFakeSessionStore()

	first, err := compute.NewAgent(compute.AgentConfig{
		Provider: compute.NewMockProvider(compute.MockResponse{Content: "nice to meet you"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	url := startRESTWithSessions(t, first, store)
	postMessage(t, url, messageRequest{Message: "my name is james", TurnID: "t1", SessionID: "s1"})

	// New server, new agent, new in-memory cache — only the store
	// carries over, exactly as it would across a process restart.
	provider := compute.NewMockProvider(compute.MockResponse{Content: "you are james"})
	second, err := compute.NewAgent(compute.AgentConfig{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	url2 := startRESTWithSessions(t, second, store)
	postMessage(t, url2, messageRequest{Message: "what is my name?", TurnID: "t2", SessionID: "s1"})

	calls := provider.Calls()
	if len(calls) != 1 {
		t.Fatalf("second server's provider saw %d calls, want 1", len(calls))
	}
	var joined string
	for _, m := range calls[0].Messages {
		joined += m.Role + ":" + m.Content + " | "
	}
	if !strings.Contains(joined, "my name is james") {
		t.Errorf("conversation did not survive restart; messages were: %s", joined)
	}
}

func TestRESTSessionsAreIsolatedFromEachOther(t *testing.T) {
	t.Parallel()
	provider := compute.NewMockProvider(
		compute.MockResponse{Content: "reply a"},
		compute.MockResponse{Content: "reply b"},
	)
	agent, err := compute.NewAgent(compute.AgentConfig{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	url := startRESTWithSessions(t, agent, newFakeSessionStore())

	postMessage(t, url, messageRequest{Message: "alice speaking", TurnID: "t1", SessionID: "alice"})
	postMessage(t, url, messageRequest{Message: "bob speaking", TurnID: "t2", SessionID: "bob"})

	calls := provider.Calls()
	for _, m := range calls[1].Messages {
		if strings.Contains(m.Content, "alice") {
			t.Errorf("bob's session saw alice's message: %+v", calls[1].Messages)
		}
	}
}

// A node with no memory function wired has a nil store; the endpoint
// must still work, just without persistence.
func TestRESTSessionIDWithoutStoreDoesNotFail(t *testing.T) {
	t.Parallel()
	agent, err := compute.NewAgent(compute.AgentConfig{
		Provider: compute.NewMockProvider(compute.MockResponse{Content: "ok"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	url := startRESTWithSessions(t, agent, nil)
	got := postMessage(t, url, messageRequest{Message: "hello", TurnID: "t1", SessionID: "s1"})
	if got.Reply != "ok" {
		t.Errorf("reply = %q, want ok", got.Reply)
	}
}

// --- session ownership -------------------------------------------------

// startRESTWithAuthAndSessions is startRESTWithSessions plus a real
// HS256 validator, so a test can drive the same server as two distinct
// authenticated identities.
func startRESTWithAuthAndSessions(t *testing.T, agent *compute.Agent, sessions SessionStore) string {
	t.Helper()
	validator, err := auth.NewValidator(auth.Config{
		AllowHS256:  true,
		HS256Secret: restTestSecret,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(RESTConfig{
		Addr:         "127.0.0.1:0",
		JWTValidator: validator,
		RequireAuth:  true,
		Sessions:     sessions,
	}, agent)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = srv.Start(ctx)
	})
	deadline := time.Now().Add(time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.Addr() == "" {
		cancel()
		wg.Wait()
		t.Fatal("server didn't bind within 1s")
	}
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return "http://" + srv.Addr()
}

// mintJWTForUser is mintValidJWT with a caller-chosen subject, so a
// test can hold two identities at once.
func mintJWTForUser(t *testing.T, sub string) string {
	t.Helper()
	tok := jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, jwtlib.MapClaims{
		"sub":   sub,
		"scope": "admin",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	s, err := tok.SignedString([]byte(restTestSecret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// postMessageAs posts as a given bearer token and returns the status
// code alongside the decoded body, so tests can assert on rejections.
func postMessageAs(t *testing.T, url, token string, body messageRequest) (int, messageResponse) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out messageResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, out
}

// Session ids are client-chosen, so two callers will collide on the
// obvious ones ("default", "1", "chat"). A collision must not hand one
// caller the other's conversation.
func TestRESTSessionIDIsScopedToItsOwner(t *testing.T) {
	t.Parallel()
	provider := compute.NewMockProvider(
		compute.MockResponse{Content: "reply to alice"},
		compute.MockResponse{Content: "reply to bob"},
	)
	agent, err := compute.NewAgent(compute.AgentConfig{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	url := startRESTWithAuthAndSessions(t, agent, newFakeSessionStore())

	if code, _ := postMessageAs(t, url, mintJWTForUser(t, "alice"),
		messageRequest{Message: "alice's private plan", TurnID: "t1", SessionID: "default"}); code != http.StatusOK {
		t.Fatalf("alice's turn: status %d", code)
	}
	if code, _ := postMessageAs(t, url, mintJWTForUser(t, "bob"),
		messageRequest{Message: "what did we discuss?", TurnID: "t2", SessionID: "default"}); code != http.StatusOK {
		t.Fatalf("bob's turn: status %d", code)
	}

	calls := provider.Calls()
	if len(calls) != 2 {
		t.Fatalf("provider saw %d calls, want 2", len(calls))
	}
	for _, m := range calls[1].Messages {
		if strings.Contains(m.Content, "alice's private plan") {
			t.Fatalf("bob's turn replayed alice's conversation: %+v", calls[1].Messages)
		}
	}
}

// The same identity keeps its own thread across turns — the scoping
// must not be so aggressive that it breaks the feature.
func TestRESTScopedSessionStillCarriesOwnHistory(t *testing.T) {
	t.Parallel()
	provider := compute.NewMockProvider(
		compute.MockResponse{Content: "first reply"},
		compute.MockResponse{Content: "second reply"},
	)
	agent, err := compute.NewAgent(compute.AgentConfig{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	url := startRESTWithAuthAndSessions(t, agent, newFakeSessionStore())
	token := mintJWTForUser(t, "alice")

	postMessageAs(t, url, token, messageRequest{Message: "my name is james", TurnID: "t1", SessionID: "default"})
	postMessageAs(t, url, token, messageRequest{Message: "what is my name?", TurnID: "t2", SessionID: "default"})

	calls := provider.Calls()
	if len(calls) != 2 {
		t.Fatalf("provider saw %d calls, want 2", len(calls))
	}
	var joined []string
	for _, m := range calls[1].Messages {
		joined = append(joined, m.Content)
	}
	if !strings.Contains(strings.Join(joined, " | "), "my name is james") {
		t.Errorf("owner lost their own history: %v", joined)
	}
}

// A ':' would be rejected by the store's key validation and silently
// cost the caller their conversation, so it's refused up front.
func TestRESTRejectsSessionIDWithSeparators(t *testing.T) {
	t.Parallel()
	agent, err := compute.NewAgent(compute.AgentConfig{
		Provider: compute.NewMockProvider(compute.MockResponse{Content: "ok"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	url := startRESTWithAuthAndSessions(t, agent, newFakeSessionStore())
	token := mintJWTForUser(t, "alice")

	for _, id := range []string{"rest:other", "alice/default", ":", "a/b"} {
		code, _ := postMessageAs(t, url, token, messageRequest{Message: "hi", TurnID: "t1", SessionID: id})
		if code != http.StatusBadRequest {
			t.Errorf("session_id %q: status %d, want 400", id, code)
		}
	}
}

// The owner prefix has to be injective: if two distinct user ids can
// produce the same prefix, the isolation above is decorative.
func TestSessionOwnerPrefixIsInjective(t *testing.T) {
	t.Parallel()
	ids := []string{"alice", "a:b", "a%3Ab", "a%b", "%", ":", "", "a%253Ab"}
	seen := map[string]string{}
	for _, id := range ids {
		got := restSessionID(id, "default")
		if prev, dup := seen[got]; dup {
			t.Errorf("user ids %q and %q both map to %q", prev, id, got)
		}
		seen[got] = id
	}
	if strings.Contains(restSessionID("a:b", "default"), ":") {
		t.Error("composed session id still contains the store's separator")
	}
}
