package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// The console's per-bot chat goes through turn.Runner as the named
// bot, streams a reply, and authenticates before the 200 — the three
// things /v1/messages does not do for a specific bot.
func TestBotChatStreamsAReplyForTheRequestedBot(t *testing.T) {
	t.Parallel()
	runner := &captureRunner{}
	srv := startWebREST(t, runner, func(c *RESTConfig) {
		c.Bots = stubBots{rec: &lobslawv1.BotRecord{Id: "coordinator", Owner: "user:alice", Enabled: true}}
	})
	tok := mintJWTWith(t, "alice@idp", nil)

	resp := doJSON(t, http.MethodPost,
		webBaseURL(srv)+"/v1/bots/coordinator/messages",
		`{"message":"hello"}`,
		http.Header{"Authorization": {"Bearer " + tok}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST bot messages = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, "event: reply") || !strings.Contains(body, `"text":"ok"`) {
		t.Fatalf("stream did not carry the reply:\n%s", body)
	}

	got := runner.lastRequest()
	if got.BotID != "coordinator" {
		t.Errorf("turn BotID = %q, want coordinator", got.BotID)
	}
	if got.Principal != identity.Bot("coordinator") {
		t.Errorf("turn Principal = %q, want %q", got.Principal, identity.Bot("coordinator"))
	}
	if got.Channel != botChannel || got.ChannelID != "coordinator" {
		t.Errorf("turn channel = %q:%q, want bot:coordinator", got.Channel, got.ChannelID)
	}
}

// Teams being enabled must not turn conversational messages into queue work,
// either locally or through the typed console proxy, for any kind of bot.
type conversationalBotRunner struct{ captureRunner }

func (c *conversationalBotRunner) Run(ctx context.Context, req turn.Request) (*turn.Response, error) {
	response, err := c.captureRunner.Run(ctx, req)
	if err != nil {
		return nil, err
	}
	response.Messages = append([]turn.Message{{Role: "system", Content: "system"}}, req.ConversationHistory...)
	response.TurnStartIndex = len(response.Messages)
	response.Messages = append(response.Messages, turn.Message{Role: "user", Content: req.Message}, turn.Message{Role: "assistant", Content: response.Reply})
	return response, nil
}

func TestBotChatsRemainConversationalWithTasksEnabled(t *testing.T) {
	t.Parallel()
	for _, remote := range []bool{false, true} {
		for _, coordinator := range []bool{false, true} {
			t.Run(fmt.Sprintf("remote=%t/coordinator=%t", remote, coordinator), func(t *testing.T) {
				t.Parallel()
				runner := &conversationalBotRunner{}
				backend := startWebREST(t, runner, func(c *RESTConfig) {
					c.Bots = stubBots{rec: &lobslawv1.BotRecord{Id: "bot", Owner: "user:alice", Enabled: true, IsCoordinator: coordinator}}
					c.StartBotTask = func(context.Context, turn.Request) (*lobslawv1.TaskApprovalRecord, error) {
						t.Error("ordinary chat created a task")
						return nil, fmt.Errorf("unexpected task")
					}
				})
				front := backend
				if remote {
					client := testConsoleClient(t, backend)
					front = startWebREST(t, nil, func(c *RESTConfig) { c.RemoteConsole = client })
				}
				for index, message := range []string{"Hello!", "What do you think about this approach?", "Can you explain how backups work?"} {
					response := doJSON(t, http.MethodPost, webBaseURL(front)+"/v1/bots/bot/messages", fmt.Sprintf(`{"message":%q}`, message), http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}})
					raw, err := io.ReadAll(response.Body)
					_ = response.Body.Close()
					if err != nil || !strings.Contains(string(raw), "event: reply") || runner.lastRequest().Message != message {
						t.Fatalf("chat was not handled conversationally: %s, %v", raw, err)
					}
					if history := runner.lastRequest().ConversationHistory; len(history) != index*2 || (index > 0 && history[0].Content != "Hello!") {
						t.Fatalf("conversation history lost: %+v", history)
					}
				}
			})
		}
	}
}

func TestBotChatOutlivesHTTPReadTimeout(t *testing.T) {
	t.Parallel()
	runner := &captureRunner{hold: make(chan struct{})}
	server := startWebREST(t, runner, func(c *RESTConfig) {
		c.ReadTimeout = 25 * time.Millisecond
		c.Bots = stubBots{rec: &lobslawv1.BotRecord{Id: "bot", Owner: "user:alice", Enabled: true}}
	})
	timer := time.AfterFunc(150*time.Millisecond, func() { close(runner.hold) })
	defer timer.Stop()
	response := doJSON(t, http.MethodPost, webBaseURL(server)+"/v1/bots/bot/messages", `{"message":"Hello!"}`, http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}})
	raw, err := io.ReadAll(response.Body)
	if err != nil || !strings.Contains(string(raw), "event: reply") {
		t.Fatalf("stream cancelled by HTTP read timeout: %s, %v", raw, err)
	}
}

// The read-only insight panes answer with the shapes the console
// parses, rather than 404s, when the registries are wired.
func TestConsoleConfigAndBotInsights(t *testing.T) {
	t.Parallel()
	srv := startWebREST(t, &captureRunner{}, func(c *RESTConfig) {
		c.Bots = stubBots{rec: &lobslawv1.BotRecord{Id: "coordinator", Owner: "user:alice", Enabled: true}}
		c.Config = &ConfigView{NodeID: "node-1", Functions: []string{"compute"}}
		c.Routines = fakeRoutines{}
		c.Memory = fakeMemory{}
		c.Transcripts = fakeTranscripts{}
	})
	tok := mintJWTWith(t, "alice@idp", nil)
	auth := http.Header{"Authorization": {"Bearer " + tok}}

	for _, path := range []string{
		"/v1/config",
		"/v1/bots/coordinator/routines",
		"/v1/bots/coordinator/memory",
		"/v1/bots/coordinator/sessions",
		"/v1/sessions/bot:coordinator",
	} {
		resp := doJSON(t, http.MethodGet, webBaseURL(srv)+path, "", auth)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
	}
}

type fakeRoutines struct{}

func (fakeRoutines) TasksForOwner(string) ([]*lobslawv1.ScheduledTaskRecord, error) {
	return nil, nil
}

type fakeMemory struct{}

func (fakeMemory) RecordsForOwner(_ context.Context, _ string, _ int) ([]MemoryRecordView, int, error) {
	return nil, 0, nil
}

type fakeTranscripts struct{}

func (fakeTranscripts) ListFiltered(_ context.Context, _, _ string) ([]*lobslawv1.SessionRecord, error) {
	return []*lobslawv1.SessionRecord{{Id: "bot:coordinator", Channel: "bot", ChannelId: "coordinator", UserId: "alice"}}, nil
}

func (fakeTranscripts) LoadMessages(_ context.Context, _ string) ([]*lobslawv1.SessionMessage, error) {
	return nil, nil
}
