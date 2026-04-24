package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// stubAgent drives WebhookHandler tests without spinning up a real
// LLM — it asserts the prompt the handler extracted and returns a
// canned reply so the test can check response shape.
type stubAgent struct {
	lastPrompt string
	reply      string
}

func (s *stubAgent) RunToolCallLoop(_ context.Context, req compute.ProcessMessageRequest) (*compute.ProcessMessageResponse, error) {
	s.lastPrompt = req.Message
	return &compute.ProcessMessageResponse{Reply: s.reply}, nil
}

func TestWebhookRejectsEmptySharedSecret(t *testing.T) {
	t.Parallel()
	_, err := NewWebhookHandler(WebhookConfig{Name: "x"}, nil)
	if err == nil {
		t.Error("empty shared secret must be refused")
	}
}

func TestWebhookRejectsMissingAuth(t *testing.T) {
	t.Parallel()
	h := mustNewWebhookWithStub(t, "secret", "ok").h
	r := httptest.NewRequest(http.MethodPost, "/webhook/x", strings.NewReader("hello"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d want 401", w.Code)
	}
}

func TestWebhookRejectsWrongAuth(t *testing.T) {
	t.Parallel()
	h := mustNewWebhookWithStub(t, "secret", "ok").h
	r := httptest.NewRequest(http.MethodPost, "/webhook/x", strings.NewReader("hello"))
	r.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d want 401", w.Code)
	}
}

func TestWebhookRejectsGET(t *testing.T) {
	t.Parallel()
	h := mustNewWebhookWithStub(t, "secret", "ok").h
	r := httptest.NewRequest(http.MethodGet, "/webhook/x", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status=%d want 405", w.Code)
	}
}

func TestWebhookJSONPromptRoundtrip(t *testing.T) {
	t.Parallel()
	env := mustNewWebhookWithStub(t, "secret", "response body")
	body, _ := json.Marshal(map[string]any{"prompt": "what's the weather?"})
	r := httptest.NewRequest(http.MethodPost, "/webhook/x", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if env.stub.lastPrompt != "what's the weather?" {
		t.Errorf("prompt=%q; want 'what's the weather?'", env.stub.lastPrompt)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["reply"] != "response body" {
		t.Errorf("reply=%v; want 'response body'", resp["reply"])
	}
}

func TestWebhookPlaintextBody(t *testing.T) {
	t.Parallel()
	env := mustNewWebhookWithStub(t, "secret", "ok")
	r := httptest.NewRequest(http.MethodPost, "/webhook/x", strings.NewReader("just some text"))
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	env.h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if env.stub.lastPrompt != "just some text" {
		t.Errorf("prompt=%q", env.stub.lastPrompt)
	}
}

func TestWebhookPathPrefixDefault(t *testing.T) {
	t.Parallel()
	h, _ := NewWebhookHandler(WebhookConfig{Name: "zapier", SharedSecret: "s"}, nil)
	if h.PathPrefix() != "/webhook/zapier" {
		t.Errorf("default path: %q", h.PathPrefix())
	}
}

func TestWebhookPathPrefixOverride(t *testing.T) {
	t.Parallel()
	h, _ := NewWebhookHandler(WebhookConfig{Name: "x", Path: "/hooks/inbound/foo", SharedSecret: "s"}, nil)
	if h.PathPrefix() != "/hooks/inbound/foo" {
		t.Errorf("override: %q", h.PathPrefix())
	}
}

type webhookTestEnv struct {
	h    *WebhookHandler
	stub *stubAgent
}

func mustNewWebhookWithStub(t *testing.T, secret, reply string) webhookTestEnv {
	t.Helper()
	stub := &stubAgent{reply: reply}
	// Use a shim compute.Agent so the handler has a typed *compute.Agent;
	// the test goes through the interface-shaped RunToolCallLoop we
	// injected via agentShim.
	h := &WebhookHandler{
		cfg:   WebhookConfig{Name: "x", SharedSecret: secret, Scope: "test"},
		agent: agentFromStub(stub),
		log:   slog.Default(),
	}
	return webhookTestEnv{h: h, stub: stub}
}

// agentFromStub builds a minimal *compute.Agent whose RunToolCallLoop
// delegates to the stub. Wraps the stub in an agentShim instance.
func agentFromStub(s *stubAgent) *compute.Agent {
	a, _ := compute.NewAgent(compute.AgentConfig{
		Provider: &shimProvider{stub: s},
	})
	return a
}

// shimProvider + the agent's usual loop produce a plain reply for
// the stub's queued content with no tool calls.
type shimProvider struct{ stub *stubAgent }

func (p *shimProvider) Chat(_ context.Context, req compute.ChatRequest) (*compute.ChatResponse, error) {
	// Last user message is the prompt the webhook extracted.
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			p.stub.lastPrompt = req.Messages[i].Content
			break
		}
	}
	return &compute.ChatResponse{Content: p.stub.reply, FinishReason: "stop"}, nil
}

// ensure types import stays live even if the stub path doesn't reach it.
var _ = types.ErrInvalidConfig
