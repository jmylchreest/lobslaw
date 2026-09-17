package gateway

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// slowTurns stands in for a bot turn that takes real time, because
// that is the only thing that reproduces the bug.
type slowTurns struct{ took time.Duration }

func (s slowTurns) Run(ctx context.Context, _ compute.TurnRequest) (*compute.ProcessMessageResponse, error) {
	select {
	case <-time.After(s.took):
		return &compute.ProcessMessageResponse{Reply: "the answer you waited for"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// A turn slower than the server's WriteTimeout must still reach the
// person who asked for it.
//
// WriteTimeout is an absolute deadline from the start of the response,
// not an inactivity timeout, so the SSE heartbeat does NOT extend it.
// With the default 60s bound, every bot turn that ran longer — routine
// against a real model, and 159s for one that delegated to three
// specialists — had its stream cut. The turn completed and was
// recorded; only the browser was told "network error". That split is
// what makes it dangerous: the work happened and the UI said it did
// not, so the obvious response is to ask for it a second time.
//
// Driven through a real httptest server rather than a ResponseRecorder
// on purpose: a recorder has no connection and therefore no deadline,
// so it cannot fail this way and would pass either side of the fix.
func TestChatSurvivesATurnLongerThanTheWriteTimeout(t *testing.T) {
	t.Parallel()

	const writeTimeout = 150 * time.Millisecond
	s := NewServer(RESTConfig{
		Turns:        slowTurns{took: 4 * writeTimeout},
		DefaultScope: "owner",
	}, nil)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			s.handleBotChat(w, r, "coordinator")
		}))
	srv.Config.WriteTimeout = writeTimeout
	srv.Start()
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/bots/coordinator/messages",
		"application/json", strings.NewReader(`{"message":"how long can you take?"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var got []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		got = append(got, scanner.Text())
	}
	stream := strings.Join(got, "\n")

	// A cut connection shows up as a read error on the body, not as a
	// status — the 200 and the headers left before the turn began.
	if err := scanner.Err(); err != nil {
		t.Fatalf("stream was cut mid-turn (%v); received so far:\n%s", err, stream)
	}
	if !strings.Contains(stream, "the answer you waited for") {
		t.Errorf("the reply never arrived; stream was:\n%s", stream)
	}
}

// confirmingTurns asks once, then succeeds — driving the SSE writer
// from the same goroutine the real runner uses.
type confirmingTurns struct{ asked bool }

func (c *confirmingTurns) Run(ctx context.Context, req compute.TurnRequest) (*compute.ProcessMessageResponse, error) {
	if req.Confirm != nil && !c.asked {
		c.asked = true
		ok, err := req.Confirm(ctx, "shell_command is guarded", "shell", "rm -rf /tmp/x")
		if err != nil {
			return nil, err
		}
		if !ok {
			return &compute.ProcessMessageResponse{Reply: "declined"}, nil
		}
	}
	// Deltas and the reply both write to the same stream the
	// confirmation just wrote to.
	if req.OnDelta != nil {
		req.OnDelta("part one ")
		req.OnDelta("part two")
	}
	return &compute.ProcessMessageResponse{Reply: "part one part two"}, nil
}

// autoPrompts approves everything the moment it is asked.
type autoPrompts struct{}

func (a *autoPrompts) Create(NewPrompt) (*Prompt, error)                 { return &Prompt{ID: "p1"}, nil }
func (a *autoPrompts) Get(string) (*Prompt, error)                       { return &Prompt{ID: "p1"}, nil }
func (a *autoPrompts) Resolve(string, PromptDecision, PromptScope) error { return nil }
func (a *autoPrompts) Wait(context.Context, string) (PromptDecision, error) {
	return PromptApproved, nil
}

// Asking for approval must not kill the node.
//
// The first version of the confirmation path locked the SSE mutex at
// the call sites, and one Lock went missing in an edit while its
// Unlock stayed. The result was "fatal error: sync: unlock of unlocked
// mutex" — not a failed turn, not a 500, but the whole process gone,
// the first time anybody clicked Approve. A fatal error is not
// recoverable and no handler-level test of the happy path would have
// seen it, which is why this drives the confirm path specifically.
//
// Run this package with -race to also catch the interleaving the mutex
// exists to prevent.
func TestAskingForApprovalDoesNotCrashTheStream(t *testing.T) {
	t.Parallel()

	s := NewServer(RESTConfig{
		Turns:           &confirmingTurns{},
		Prompts:         &autoPrompts{},
		ConfirmationTTL: time.Minute,
		DefaultScope:    "owner",
	}, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleBotChat(w, r, "engineering")
	}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/bots/engineering/messages",
		"application/json", strings.NewReader(`{"message":"write a file"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	stream := string(raw)

	for _, want := range []string{"needs_confirmation", "delta", "part one part two"} {
		if !strings.Contains(stream, want) {
			t.Errorf("stream is missing %q:\n%s", want, stream)
		}
	}
}

// An unauthenticated chat request gets a 401, not a streamed error.
//
// Authentication used to happen inside runBotTurn — after the 200, the
// SSE headers and a `start` event were already on the wire. The caller
// was refused, but anything speaking HTTP rather than SSE saw a
// successful request, and the refusal arrived as stream content.
func TestChatRefusesBeforeOpeningTheStream(t *testing.T) {
	t.Parallel()

	key, err := DeriveConsoleKey([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("DeriveConsoleKey: %v", err)
	}
	s := NewServer(RESTConfig{
		ConsoleKey:   key,
		ConsoleToken: "shared-secret",
		RequireAuth:  true,
		Turns:        slowTurns{took: time.Millisecond},
		DefaultScope: "owner",
	}, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/bots/coordinator/messages",
		strings.NewReader(`{"message":"hello"}`))
	s.handleBotChat(rec, req, "coordinator")

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "event-stream") {
		t.Errorf("Content-Type = %q — the stream was opened for a caller who was refused", ct)
	}
	if strings.Contains(rec.Body.String(), "event: start") {
		t.Error("a start event was emitted before the caller was authenticated")
	}
}
