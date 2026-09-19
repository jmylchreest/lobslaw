package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// End-to-end handler tests, the Slack counterpart of
// TestTelegramMessageDispatchesToAgent and friends.
//
// Slack's inbound is a websocket rather than HTTP, so these drive
// handleEvent directly and assert on what reaches the Web API — which
// is the same seam the Telegram tests use, entered one layer lower.

// slackHarness captures every Web API call the handler makes.
type slackHarness struct {
	h   *SlackHandler
	srv *httptest.Server

	mu    sync.Mutex
	calls []slackCall
}

type slackCall struct {
	Method string
	Body   map[string]any
}

func newSlackHarness(t *testing.T, agent *compute.Agent, cfg SlackConfig) *slackHarness {
	t.Helper()
	hh := &slackHarness{}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)

		hh.mu.Lock()
		hh.calls = append(hh.calls, slackCall{
			Method: strings.TrimPrefix(r.URL.Path, "/"),
			Body:   body,
		})
		hh.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		// ok:true with a ts, so postMessageTS has something to record.
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1700000000.000001"}`))
	})
	hh.srv = httptest.NewServer(mux)
	t.Cleanup(hh.srv.Close)

	cfg.BotToken = "xoxb-test"
	cfg.AppToken = "xapp-test"
	cfg.APIBase = hh.srv.URL
	cfg.HTTPClient = hh.srv.Client()
	cfg.Logger = discardLogger()

	h, err := NewSlackHandler(cfg, compute.Adapt(agent))
	if err != nil {
		t.Fatalf("NewSlackHandler: %v", err)
	}
	h.botUserID = "U0BOT"
	h.teamID = "T0TEAM"
	hh.h = h
	return hh
}

func (hh *slackHarness) callsTo(method string) []slackCall {
	hh.mu.Lock()
	defer hh.mu.Unlock()
	var out []slackCall
	for _, c := range hh.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// posted returns the text of every message the handler sent, whether it
// posted or updated one.
func (hh *slackHarness) posted() []string {
	hh.mu.Lock()
	defer hh.mu.Unlock()
	var out []string
	for _, c := range hh.calls {
		switch c.Method {
		case "chat.postMessage", "chat.update":
			if s, ok := c.Body["text"].(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func dmEvent(text string) slackEventsPayload {
	return slackEventsPayload{
		TeamID: "T0TEAM",
		Event: slackEvent{
			Type: "message", ChannelType: "im",
			User: "U0ALICE", Channel: "D0ALICE", TS: "1700000000.000100",
			Text: text,
		},
	}
}

func TestSlackMessageDispatchesToAgent(t *testing.T) {
	t.Parallel()

	hh := newSlackHarness(t, mockAgent(t, compute.MockResponse{Content: "pong"}),
		SlackConfig{AllowedChannels: []string{"*"}, UnknownUserScope: "public"})

	hh.h.handleEvent(context.Background(), dmEvent("ping"))

	got := hh.posted()
	if len(got) == 0 {
		t.Fatal("no message was sent for a well-formed event")
	}
	if got[len(got)-1] != "pong" {
		t.Errorf("final message = %q, want the agent's reply", got[len(got)-1])
	}
}

// The allowlist is the coarse gate, and its refusal is deliberately
// silent — a bot that announces a refusal in a room it was not
// permitted to speak in has just spoken in that room.
func TestSlackDisallowedChannelSendsNothing(t *testing.T) {
	t.Parallel()

	hh := newSlackHarness(t, mockAgent(t, compute.MockResponse{Content: "should not run"}),
		SlackConfig{AllowedChannels: []string{"C0OTHER"}, UnknownUserScope: "public"})

	hh.h.handleEvent(context.Background(), dmEvent("ping"))

	if got := hh.posted(); len(got) != 0 {
		t.Fatalf("a refused conversation produced output: %q", got)
	}
}

func TestSlackUnknownUserSendsNothing(t *testing.T) {
	t.Parallel()

	// No user_scopes and no fallthrough: the user is unmapped.
	hh := newSlackHarness(t, mockAgent(t, compute.MockResponse{Content: "should not run"}),
		SlackConfig{AllowedChannels: []string{"*"}})

	hh.h.handleEvent(context.Background(), dmEvent("ping"))

	if got := hh.posted(); len(got) != 0 {
		t.Fatalf("an unmapped user produced output: %q", got)
	}
}

// Slack redelivers anything it did not see acked, and a dropped ack is
// normal during a reconnect. Without dedup the turn re-runs, tool calls
// and all.
func TestSlackDuplicateEventRunsOnce(t *testing.T) {
	t.Parallel()

	hh := newSlackHarness(t,
		mockAgent(t, compute.MockResponse{Content: "first"}, compute.MockResponse{Content: "second"}),
		SlackConfig{AllowedChannels: []string{"*"}, UnknownUserScope: "public"})

	ev := dmEvent("ping")
	hh.h.handleEvent(context.Background(), ev)
	hh.h.handleEvent(context.Background(), ev)

	// Exactly one turn's worth of output; the second delivery is dropped
	// before the agent is reached.
	for _, text := range hh.posted() {
		if text == "second" {
			t.Fatal("a redelivered event ran the turn a second time")
		}
	}
}

// A DM answers inline. A channel answers in a thread, so the room is
// not filled with one person's conversation with the bot.
func TestSlackChannelRepliesInThread(t *testing.T) {
	t.Parallel()

	hh := newSlackHarness(t, mockAgent(t, compute.MockResponse{Content: "pong"}),
		SlackConfig{AllowedChannels: []string{"*"}, UnknownUserScope: "public"})

	hh.h.handleEvent(context.Background(), slackEventsPayload{
		TeamID: "T0TEAM",
		Event: slackEvent{
			Type: "message", ChannelType: "channel",
			User: "U0ALICE", Channel: "C0GENERAL", TS: "1700000000.000100",
			Text: "ping",
		},
	})

	var threaded bool
	for _, c := range hh.callsTo("chat.postMessage") {
		if ts, ok := c.Body["thread_ts"].(string); ok && ts == "1700000000.000100" {
			threaded = true
		}
	}
	if !threaded {
		t.Error("a channel reply was not threaded off the triggering message")
	}
}

// The bot must not answer itself. In a channel that does not terminate.
func TestSlackIgnoresItsOwnMessage(t *testing.T) {
	t.Parallel()

	hh := newSlackHarness(t, mockAgent(t, compute.MockResponse{Content: "should not run"}),
		SlackConfig{AllowedChannels: []string{"*"}, UnknownUserScope: "public"})

	ev := dmEvent("ping")
	ev.Event.User = "U0BOT"
	hh.h.handleEvent(context.Background(), ev)

	if got := hh.posted(); len(got) != 0 {
		t.Fatalf("the bot answered itself: %q", got)
	}
}

// A slash command must not take a turn: no lease, no transcript, no
// model call. It is answered from the dispatcher alone.
func TestSlackSlashCommandDoesNotReachTheAgent(t *testing.T) {
	t.Parallel()

	hh := newSlackHarness(t, mockAgent(t, compute.MockResponse{Content: "agent ran"}),
		SlackConfig{
			AllowedChannels:   []string{"*"},
			UserScopes:        map[string]string{"U0ALICE": "owner"},
			CommandAuthorizer: fakeAuthz{allow: true},
		})

	hh.h.handleSlashCommand(context.Background(), slackSlashCommand{
		Command: "/lobslaw", Text: "help",
		UserID: "U0ALICE", TeamID: "T0TEAM", ChannelID: "D0ALICE",
	})

	eph := hh.callsTo("chat.postEphemeral")
	if len(eph) != 1 {
		t.Fatalf("expected one ephemeral reply, got %d", len(eph))
	}
	text, _ := eph[0].Body["text"].(string)
	if !strings.Contains(text, "whoami") {
		t.Errorf("help output = %q, want the command list", text)
	}
	for _, c := range hh.calls {
		if c.Method == "chat.postMessage" {
			t.Error("a slash command posted to the conversation instead of answering the caller")
		}
	}
}
