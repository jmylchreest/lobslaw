package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/compute/drivertest"
)

// fakeAPI serves a canned Messages response, and records the last
// request body so the translation can be asserted on.
type fakeAPI struct {
	srv      *httptest.Server
	lastBody []byte
	lastHdr  http.Header
}

func newFakeAPI(t *testing.T, status int, body string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastBody, _ = readAll(r)
		f.lastHdr = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

const okResponse = `{
  "content":[{"type":"text","text":"pong"}],
  "stop_reason":"end_turn",
  "usage":{"input_tokens":11,"output_tokens":3,"cache_read_input_tokens":2}
}`

func newDriver(t *testing.T, endpoint string) *Driver {
	t.Helper()
	d, err := New(Config{
		Endpoint:   endpoint,
		Model:      "claude-test",
		Credential: compute.NewHeaderCredential("x-api-key", "test-key"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The whole point of a second driver: it must satisfy the same
// contract as the first without the first's shape leaking into it.
func TestConformance(t *testing.T) {
	t.Parallel()
	ok := newFakeAPI(t, http.StatusOK, okResponse)

	drivertest.Run(t, drivertest.Subject{
		Name: "anthropic",
		Chat: newDriver(t, ok.srv.URL),
		FailingChat: func(status int, body string) compute.ChatDriver {
			return newDriver(t, newFakeAPI(t, status, body).srv.URL)
		},
	})
}

// Anthropic requires its own auth header and a pinned API version.
// A driver that sent a bearer token would fail against the real API
// with a message that looks like a bad key rather than a bad header.
func TestSendsAnthropicHeaders(t *testing.T) {
	t.Parallel()
	f := newFakeAPI(t, http.StatusOK, okResponse)
	d := newDriver(t, f.srv.URL)

	if _, err := d.Chat(context.Background(), compute.ChatRequest{
		Messages: []compute.Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.lastHdr.Get("x-api-key"); got != "test-key" {
		t.Errorf("x-api-key = %q, want test-key", got)
	}
	if got := f.lastHdr.Get("Authorization"); got != "" {
		t.Errorf("sent an Authorization header (%q); Anthropic uses x-api-key", got)
	}
	if got := f.lastHdr.Get("anthropic-version"); got != apiVersion {
		t.Errorf("anthropic-version = %q, want %q", got, apiVersion)
	}
}

// The three structural differences from the OpenAI shape. Each one
// would produce a 400 from the real API, so each is worth pinning.
func TestTranslatesTheMessagesShape(t *testing.T) {
	t.Parallel()
	f := newFakeAPI(t, http.StatusOK, okResponse)
	d := newDriver(t, f.srv.URL)

	_, err := d.Chat(context.Background(), compute.ChatRequest{
		Messages: []compute.Message{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "run it"},
			{Role: "assistant", ToolCalls: []compute.ToolCall{
				{ID: "call_1", Name: "shell", Arguments: `{"cmd":"ls"}`},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "a\nb"},
		},
		Tools: []compute.Tool{{
			Name: "shell", Description: "run", Parameters: json.RawMessage(`{"type":"object"}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var sent wireRequest
	if err := json.Unmarshal(f.lastBody, &sent); err != nil {
		t.Fatalf("request was not valid JSON: %v", err)
	}

	// 1. The system prompt is a top-level field, not a message.
	if sent.System != "be terse" {
		t.Errorf("system = %q, want it hoisted out of the message list", sent.System)
	}
	for _, m := range sent.Messages {
		if m.Role == "system" {
			t.Error("a system message survived into messages[]; Anthropic rejects that")
		}
	}

	// 2. max_tokens is mandatory — the request is rejected without it.
	if sent.MaxTokens <= 0 {
		t.Error("max_tokens missing; Anthropic requires it and has no default")
	}

	// 3. A tool result is a user message carrying a tool_result block.
	var foundResult bool
	for _, m := range sent.Messages {
		for _, b := range m.Content {
			if b.Type == "tool_result" {
				foundResult = true
				if m.Role != "user" {
					t.Errorf("tool_result sent on role %q, want user", m.Role)
				}
				if b.ToolUseID != "call_1" {
					t.Errorf("tool_use_id = %q, want call_1", b.ToolUseID)
				}
			}
		}
	}
	if !foundResult {
		t.Error("the tool result never reached the wire")
	}

	// Tool definitions use input_schema, not a nested function object.
	if len(sent.Tools) != 1 || len(sent.Tools[0].InputSchema) == 0 {
		t.Errorf("tools not translated to input_schema form: %+v", sent.Tools)
	}
}

// Cache reads are billed differently from fresh input and are reported
// separately. Folding them into input would overstate the cost of the
// long-context turns caching exists to make cheap.
func TestReportsCachedTokensSeparately(t *testing.T) {
	t.Parallel()
	f := newFakeAPI(t, http.StatusOK, okResponse)
	d := newDriver(t, f.srv.URL)

	resp, err := d.Chat(context.Background(), compute.ChatRequest{
		Messages: []compute.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.CachedTokens != 2 {
		t.Errorf("CachedTokens = %d, want 2", resp.Usage.CachedTokens)
	}
	if resp.Usage.PromptTokens != 13 {
		t.Errorf("PromptTokens = %d, want 13 (11 input + 2 cache read)", resp.Usage.PromptTokens)
	}
	if resp.Usage.TotalTokens != 16 {
		t.Errorf("TotalTokens = %d, want 16", resp.Usage.TotalTokens)
	}
}

// The agent loop branches on FinishReason using the OpenAI vocabulary.
// A driver that passed Anthropic's through would break the tool loop
// silently — "tool_use" is not "tool_calls".
func TestNormalisesStopReason(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"end_turn":      "stop",
		"stop_sequence": "stop",
		"tool_use":      "tool_calls",
		"max_tokens":    "length",
		"":              "stop",
	} {
		if got := normaliseStop(in); got != want {
			t.Errorf("normaliseStop(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRequiresCredential(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{Model: "x"}); err == nil {
		t.Error("a driver with no credential was constructed; it would fail on the first turn instead")
	}
}

func readAll(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(r.Body)
}

// The same [[compute.providers]] endpoint field must mean the same
// thing whichever driver sits beside it. The openai driver has always
// accepted a base URL; this one required the full path and answered a
// bare host with "HTTP 404: " on the first real turn — an empty-bodied
// 404 that reads as a broken vendor rather than a config typo.
func TestEndpointAcceptsABaseURLLikeTheOpenAIDriver(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		given, want string
	}{
		// What https://docs.claude.com prints, and what an operator
		// copies into config.toml.
		{"https://api.anthropic.com", "https://api.anthropic.com/v1/messages"},
		{"https://api.anthropic.com/", "https://api.anthropic.com/v1/messages"},
		{"https://api.anthropic.com/v1", "https://api.anthropic.com/v1/messages"},
		// Already complete: left exactly as given, so an operator who
		// spelled it out in full is not second-guessed.
		{"https://api.anthropic.com/v1/messages", "https://api.anthropic.com/v1/messages"},
		// A gateway on a non-standard prefix still gets the API path.
		{"https://proxy.internal/anthropic", "https://proxy.internal/anthropic/v1/messages"},
	} {
		d, err := New(Config{Endpoint: tc.given, Credential: compute.NewHeaderCredential("x-api-key", "k")})
		if err != nil {
			t.Fatalf("%s: %v", tc.given, err)
		}
		if d.endpoint != tc.want {
			t.Errorf("endpoint %q resolved to %q, want %q", tc.given, d.endpoint, tc.want)
		}
	}
}
