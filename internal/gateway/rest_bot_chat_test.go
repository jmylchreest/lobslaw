package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/identity"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// The console's per-bot chat goes through turn.Runner as the named
// bot, streams a reply, and authenticates before the 200 — the three
// things /v1/messages does not do for a specific bot.
func TestBotChatStreamsAReplyForTheRequestedBot(t *testing.T) {
	t.Parallel()
	runner := &captureRunner{}
	srv := startWebREST(t, runner, func(c *RESTConfig) {
		c.Bots = stubBots{rec: &lobslawv1.BotRecord{Id: "coordinator", Enabled: true}}
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

// The read-only insight panes answer with the shapes the console
// parses, rather than 404s, when the registries are wired.
func TestConsoleConfigAndBotInsights(t *testing.T) {
	t.Parallel()
	srv := startWebREST(t, &captureRunner{}, func(c *RESTConfig) {
		c.Bots = stubBots{rec: &lobslawv1.BotRecord{Id: "coordinator", Enabled: true}}
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
	return nil, nil
}

func (fakeTranscripts) LoadMessages(_ context.Context, _ string) ([]*lobslawv1.SessionMessage, error) {
	return nil, nil
}
