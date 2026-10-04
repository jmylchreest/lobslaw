package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestForwardedPromptIsOwnedEvenWhenBackendHTTPAuthIsOff(t *testing.T) {
	t.Parallel()
	srv := &Server{cfg: RESTConfig{RequireAuth: false}}
	if srv.promptVisible(&Prompt{RaisedFor: "bob"}, requestAuth{Claims: &types.Claims{UserID: "alice"}, FromPeer: true}) {
		t.Fatal("backend HTTP setting bypassed forwarded user authorization")
	}
}

type ownedTranscript struct{ owner string }

func (s ownedTranscript) ListFiltered(context.Context, string, string) ([]*lobslawv1.SessionRecord, error) {
	return []*lobslawv1.SessionRecord{{Id: "rest:private", Channel: "rest", ChannelId: "private", UserId: s.owner}}, nil
}
func (ownedTranscript) LoadMessages(context.Context, string) ([]*lobslawv1.SessionMessage, error) {
	return []*lobslawv1.SessionMessage{{Role: "user", Content: "private marker"}}, nil
}

func TestTranscriptAuthorizesRecordedParticipant(t *testing.T) {
	t.Parallel()
	for _, owner := range []string{"alice", "bob", ""} {
		srv := startWebREST(t, &captureRunner{}, func(c *RESTConfig) { c.Transcripts = ownedTranscript{owner: owner} })
		resp := doJSON(t, http.MethodGet, webBaseURL(srv)+"/v1/sessions/rest:private", "", http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}})
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if owner == "alice" {
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "private marker") {
				t.Fatal("owner cannot read transcript")
			}
		} else if resp.StatusCode != http.StatusForbidden || strings.Contains(string(body), "private marker") {
			t.Fatalf("transcript leaked for owner %q", owner)
		}
	}
}

func TestBotChatUsesConversationGate(t *testing.T) {
	t.Parallel()
	runner := &captureRunner{}
	srv := startWebREST(t, runner, func(c *RESTConfig) {
		c.QueueMode = QueueOff
		c.Bots = stubBots{rec: &lobslawv1.BotRecord{Id: "worker", Owner: "user:alice", Enabled: true}}
	})
	lease, _ := srv.gate.Acquire(context.Background(), cacheKey(SessionRef{Channel: botChannel, ChannelID: "worker", UserID: "alice"}), "held", "first")
	defer lease.Release()
	resp := doJSON(t, http.MethodPost, webBaseURL(srv)+"/v1/bots/worker/messages", `{"message":"second"}`, http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}})
	if resp.StatusCode != http.StatusConflict || runner.lastRequest().BotID != "" {
		t.Fatal("concurrent bot turn escaped session gate")
	}
}

func TestBotSettingsRejectStaleEditorRevision(t *testing.T) {
	t.Parallel()
	bot := &lobslawv1.BotRecord{Id: "worker", Owner: "user:alice", Revision: 7, Instructions: "current brief"}
	srv := startWebREST(t, &captureRunner{}, func(c *RESTConfig) { c.Bots = stubBots{rec: bot} })
	resp := doJSON(t, http.MethodPatch, webBaseURL(srv)+"/v1/bots/worker", `{"revision":6,"instructions":"stale brief"}`, http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}})
	if resp.StatusCode != http.StatusConflict || bot.Instructions != "current brief" {
		t.Fatal("stale editor overwrote current settings")
	}
}

func TestProxyCannotEnrollWithoutCredentials(t *testing.T) {
	t.Parallel()
	srv := startWebREST(t, &captureRunner{}, nil)
	u, err := url.Parse(webBaseURL(srv))
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	for _, path := range []string{"/v1/session", "/v1/session/code"} {
		req := httptest.NewRequest(http.MethodPost, "http://console.example"+path, strings.NewReader(`{"loopback":true}`))
		req.RemoteAddr = "203.0.113.42:12345"
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", path, w.Code)
		}
		if w.Header().Get("Set-Cookie") != "" {
			t.Error("unauthenticated client received cookie")
		}
	}
}

func TestCodeGuessBudgetCannotBeMultipliedByClientAddress(t *testing.T) {
	t.Parallel()
	store := newLoginStore()
	now := time.Now()
	for range loginCodeAttemptLimit {
		if !store.allowCodeAttempt(now) {
			t.Fatal("allowance ended early")
		}
	}
	if store.allowCodeAttempt(now) {
		t.Fatal("unlimited online code guesses")
	}
	if !store.allowCodeAttempt(now.Add(loginCodeAttemptWindow)) {
		t.Fatal("guess window did not reset")
	}
}

func TestBotRoutesRejectOtherOwnersAndUnownedRecords(t *testing.T) {
	t.Parallel()
	for _, owner := range []string{"user:bob", ""} {
		srv := startWebREST(t, &captureRunner{}, func(c *RESTConfig) {
			c.Bots = stubBots{rec: &lobslawv1.BotRecord{Id: "worker", Owner: owner, Enabled: true}}
			c.Transcripts = fakeTranscripts{}
			c.Memory = fakeMemory{}
			c.Routines = fakeRoutines{}
		})
		auth := http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}}
		for _, suffix := range []string{"", "/sessions", "/memory", "/routines", "/messages"} {
			method, body := http.MethodGet, ""
			if suffix == "/messages" {
				method, body = http.MethodPost, `{"message":"hello"}`
			}
			resp := doJSON(t, method, webBaseURL(srv)+"/v1/bots/worker"+suffix, body, auth)
			_, _ = io.Copy(io.Discard, resp.Body)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("owner=%q suffix=%q status=%d", owner, suffix, resp.StatusCode)
			}
		}
	}
}
