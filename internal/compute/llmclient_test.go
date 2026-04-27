package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeOpenAIServer spins up an httptest.Server that responds to
// /chat/completions with a given handler. Lets tests assert on the
// request shape and return whatever response shape they want to
// exercise the client's parsing / error paths.
func fakeOpenAIServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestNewLLMClientRequiresEndpoint(t *testing.T) {
	t.Parallel()
	_, err := NewLLMClient(LLMClientConfig{})
	if err == nil {
		t.Error("empty endpoint should fail fast")
	}
}

// TestNewLLMClientAppendsChatCompletions — operators copy-paste
// base URLs like "https://openrouter.ai/api/v1" from provider
// docs. The client must append /chat/completions when missing and
// leave alone when already present. Trailing slashes collapse.
func TestNewLLMClientAppendsChatCompletions(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"https://openrouter.ai/api/v1":                   "https://openrouter.ai/api/v1/chat/completions",
		"https://openrouter.ai/api/v1/":                  "https://openrouter.ai/api/v1/chat/completions",
		"https://openrouter.ai/api/v1/chat/completions":  "https://openrouter.ai/api/v1/chat/completions",
		"https://openrouter.ai/api/v1/chat/completions/": "https://openrouter.ai/api/v1/chat/completions",
	}
	for in, want := range cases {
		c, err := NewLLMClient(LLMClientConfig{Endpoint: in})
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if c.endpoint != want {
			t.Errorf("endpoint(%q) = %q; want %q", in, c.endpoint, want)
		}
	}
}

func TestLLMClientChatHappyPath(t *testing.T) {
	t.Parallel()
	srv := fakeOpenAIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method: got %s, want POST", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("auth header: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type: %q", r.Header.Get("Content-Type"))
		}
		var req openAIRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "test-model" {
			t.Errorf("model: %q", req.Model)
		}
		resp := openAIResponse{
			Choices: []openAIChoice{{
				Message: openAIResponseMessage{
					Role:    "assistant",
					Content: "hello world",
				},
				FinishReason: "stop",
			}},
			Usage: openAIUsage{
				PromptTokens:     100,
				CompletionTokens: 20,
				TotalTokens:      120,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	client, err := NewLLMClient(LLMClientConfig{
		Endpoint: srv.URL,
		APIKey:   "test-key",
		Model:    "test-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "hello world" {
		t.Errorf("content: %q", got.Content)
	}
	if got.FinishReason != "stop" {
		t.Errorf("finish reason: %q", got.FinishReason)
	}
	if got.Usage.TotalTokens != 120 {
		t.Errorf("usage.total_tokens: %d", got.Usage.TotalTokens)
	}
}

func TestLLMClientModelFallsBackToDefault(t *testing.T) {
	t.Parallel()
	var gotModel string
	srv := fakeOpenAIServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req openAIRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		_ = json.NewEncoder(w).Encode(openAIResponse{
			Choices: []openAIChoice{{Message: openAIResponseMessage{Content: "ok"}}},
		})
	})
	client, _ := NewLLMClient(LLMClientConfig{
		Endpoint: srv.URL,
		Model:    "default-model",
	})
	// Request omits Model → client fills in the default.
	_, _ = client.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if gotModel != "default-model" {
		t.Errorf("expected default-model, got %q", gotModel)
	}
}

func TestLLMClientToolCallsRoundTrip(t *testing.T) {
	t.Parallel()
	srv := fakeOpenAIServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req openAIRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Tools) != 1 {
			t.Errorf("want 1 tool, got %d", len(req.Tools))
		}
		// Tools is []any on the wire-shape struct so function +
		// server tools can coexist; JSON decode delivers a
		// map[string]any we dig into.
		first, ok := req.Tools[0].(map[string]any)
		if !ok {
			t.Fatalf("tool[0] is not an object: %T", req.Tools[0])
		}
		fn, ok := first["function"].(map[string]any)
		if !ok {
			t.Fatalf("tool[0].function missing: %+v", first)
		}
		if fn["name"] != "bash" {
			t.Errorf("tool name: %v", fn["name"])
		}
		resp := openAIResponse{
			Choices: []openAIChoice{{
				Message: openAIResponseMessage{
					Role: "assistant",
					ToolCalls: []openAIToolCall{
						{
							ID:   "call-1",
							Type: "function",
							Function: openAIToolCallFunc{
								Name:      "bash",
								Arguments: `{"cmd":"echo hi"}`,
							},
						},
					},
				},
				FinishReason: "tool_calls",
			}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	client, _ := NewLLMClient(LLMClientConfig{Endpoint: srv.URL, Model: "m"})
	got, err := client.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "run echo"}},
		Tools: []Tool{{
			Name:        "bash",
			Description: "run a shell command",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(got.ToolCalls))
	}
	if got.ToolCalls[0].ID != "call-1" {
		t.Errorf("tool call id: %q", got.ToolCalls[0].ID)
	}
	if got.ToolCalls[0].Arguments != `{"cmd":"echo hi"}` {
		t.Errorf("tool call args: %q", got.ToolCalls[0].Arguments)
	}
	if got.FinishReason != "tool_calls" {
		t.Errorf("finish reason: %q", got.FinishReason)
	}
}

func TestLLMClientAssistantToolCallMessageShape(t *testing.T) {
	t.Parallel()
	srv := fakeOpenAIServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req openAIRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		// Caller builds a 3-message conversation: user → assistant with tool call → tool result.
		if len(req.Messages) != 3 {
			t.Fatalf("want 3 messages, got %d", len(req.Messages))
		}
		assistant := req.Messages[1]
		if assistant.Role != "assistant" {
			t.Errorf("msg[1].Role: %q", assistant.Role)
		}
		if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "prev-call" {
			t.Errorf("assistant tool_calls not marshalled: %+v", assistant)
		}
		toolMsg := req.Messages[2]
		if toolMsg.Role != "tool" {
			t.Errorf("msg[2].Role: %q", toolMsg.Role)
		}
		if toolMsg.ToolCallID != "prev-call" {
			t.Errorf("msg[2].ToolCallID: %q", toolMsg.ToolCallID)
		}
		_ = json.NewEncoder(w).Encode(openAIResponse{
			Choices: []openAIChoice{{Message: openAIResponseMessage{Content: "thanks"}}},
		})
	})

	client, _ := NewLLMClient(LLMClientConfig{Endpoint: srv.URL, Model: "m"})
	_, err := client.Chat(context.Background(), ChatRequest{
		Messages: []Message{
			{Role: "user", Content: "run echo"},
			{Role: "assistant", ToolCalls: []ToolCall{
				{ID: "prev-call", Name: "bash", Arguments: `{}`},
			}},
			{Role: "tool", ToolCallID: "prev-call", Content: "hi\n"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLLMClient429RateLimitSentinel(t *testing.T) {
	t.Parallel()
	srv := fakeOpenAIServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	})
	client, _ := NewLLMClient(LLMClientConfig{Endpoint: srv.URL, Model: "m"})
	_, err := client.Chat(context.Background(), ChatRequest{})
	if !errors.Is(err, ErrLLMRateLimit) {
		t.Errorf("want ErrLLMRateLimit; got %v", err)
	}
}

func TestLLMClient401UnauthorizedSentinel(t *testing.T) {
	t.Parallel()
	srv := fakeOpenAIServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid key"}`))
	})
	client, _ := NewLLMClient(LLMClientConfig{Endpoint: srv.URL, Model: "m"})
	_, err := client.Chat(context.Background(), ChatRequest{})
	if !errors.Is(err, ErrLLMUnauthorized) {
		t.Errorf("want ErrLLMUnauthorized; got %v", err)
	}
}

func TestLLMClient403ForbiddenSentinel(t *testing.T) {
	t.Parallel()
	srv := fakeOpenAIServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	client, _ := NewLLMClient(LLMClientConfig{Endpoint: srv.URL, Model: "m"})
	_, err := client.Chat(context.Background(), ChatRequest{})
	if !errors.Is(err, ErrLLMUnauthorized) {
		t.Errorf("403 should fold into ErrLLMUnauthorized; got %v", err)
	}
}

func TestLLMClient500GenericStatus(t *testing.T) {
	t.Parallel()
	srv := fakeOpenAIServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("provider melted"))
	})
	client, _ := NewLLMClient(LLMClientConfig{Endpoint: srv.URL, Model: "m"})
	_, err := client.Chat(context.Background(), ChatRequest{})
	if !errors.Is(err, ErrLLMHTTPStatus) {
		t.Errorf("want ErrLLMHTTPStatus; got %v", err)
	}
	// The body excerpt should be in the error message for triage.
	if !strings.Contains(err.Error(), "provider melted") {
		t.Errorf("error should embed body excerpt; got %q", err.Error())
	}
}

func TestLLMClientMalformedResponse(t *testing.T) {
	t.Parallel()
	srv := fakeOpenAIServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not json at all`))
	})
	client, _ := NewLLMClient(LLMClientConfig{Endpoint: srv.URL, Model: "m"})
	_, err := client.Chat(context.Background(), ChatRequest{})
	if !errors.Is(err, ErrLLMMalformed) {
		t.Errorf("want ErrLLMMalformed; got %v", err)
	}
}

func TestLLMClientEmptyChoicesIsMalformed(t *testing.T) {
	t.Parallel()
	srv := fakeOpenAIServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(openAIResponse{Choices: nil})
	})
	client, _ := NewLLMClient(LLMClientConfig{Endpoint: srv.URL, Model: "m"})
	_, err := client.Chat(context.Background(), ChatRequest{})
	if !errors.Is(err, ErrLLMMalformed) {
		t.Errorf("empty choices list should surface as malformed; got %v", err)
	}
}

func TestLLMClientContextCancellation(t *testing.T) {
	t.Parallel()
	// Server blocks long enough for ctx to cancel first.
	srv := fakeOpenAIServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
			_ = json.NewEncoder(w).Encode(openAIResponse{
				Choices: []openAIChoice{{Message: openAIResponseMessage{Content: "late"}}},
			})
		}
	})
	client, _ := NewLLMClient(LLMClientConfig{Endpoint: srv.URL, Model: "m"})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := client.Chat(ctx, ChatRequest{})
	if err == nil {
		t.Fatal("expected ctx-deadline error")
	}
	// Either deadline-exceeded or URL error wrapping — both are fine
	// as long as the caller can see a non-nil error.
}

func TestLLMClientTruncatesLongBodyInErrors(t *testing.T) {
	t.Parallel()
	bigBody := strings.Repeat("x", 2000)
	srv := fakeOpenAIServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(bigBody))
	})
	client, _ := NewLLMClient(LLMClientConfig{Endpoint: srv.URL, Model: "m"})
	_, err := client.Chat(context.Background(), ChatRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	// Error message must NOT contain the full 2000-byte body.
	if len(err.Error()) > 2000 {
		t.Errorf("error should truncate huge bodies; len=%d", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("truncation sentinel missing: %q", err.Error())
	}
}

func TestLLMClientNoAPIKeyOmitsAuthHeader(t *testing.T) {
	t.Parallel()
	var authHeader string
	srv := fakeOpenAIServer(t, func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(openAIResponse{
			Choices: []openAIChoice{{Message: openAIResponseMessage{Content: "ok"}}},
		})
	})
	client, _ := NewLLMClient(LLMClientConfig{Endpoint: srv.URL, Model: "m"}) // no APIKey
	_, _ = client.Chat(context.Background(), ChatRequest{})
	if authHeader != "" {
		t.Errorf("empty API key should skip Authorization header; got %q", authHeader)
	}
}

func TestLLMClientSatisfiesLLMProviderInterface(t *testing.T) {
	t.Parallel()
	var _ LLMProvider = (*LLMClient)(nil)
}

// TestLLMClientUsageCachedTokensSurface confirms the CachedTokens
// field is populated when the provider sends it — matters for cost
// accounting against providers with prompt caching (Anthropic via
// proxy or OpenRouter).
func TestLLMClientUsageCachedTokensSurface(t *testing.T) {
	t.Parallel()
	srv := fakeOpenAIServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = json.NewEncoder(w).Encode(openAIResponse{
			Choices: []openAIChoice{{Message: openAIResponseMessage{Content: "ok"}}},
			Usage: openAIUsage{
				PromptTokens:     100,
				CompletionTokens: 20,
				TotalTokens:      120,
				CachedTokens:     80,
			},
		})
	})
	client, _ := NewLLMClient(LLMClientConfig{Endpoint: srv.URL, Model: "m"})
	got, err := client.Chat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Usage.CachedTokens != 80 {
		t.Errorf("cached_tokens: got %d, want 80", got.Usage.CachedTokens)
	}
}

// TestLLMClientToolChoiceForwarded — the tool_choice field is an
// optional passthrough; tests that build requests with it should
// see the value on the wire.
func TestLLMClientToolChoiceForwarded(t *testing.T) {
	t.Parallel()
	var gotChoice string
	srv := fakeOpenAIServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req openAIRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if s, ok := req.ToolChoice.(string); ok {
			gotChoice = s
		}
		_ = json.NewEncoder(w).Encode(openAIResponse{
			Choices: []openAIChoice{{Message: openAIResponseMessage{Content: "ok"}}},
		})
	})
	client, _ := NewLLMClient(LLMClientConfig{Endpoint: srv.URL, Model: "m"})
	_, _ = client.Chat(context.Background(), ChatRequest{ToolChoice: "required"})
	if gotChoice != "required" {
		t.Errorf("tool_choice forwarding: got %q, want 'required'", gotChoice)
	}
}

// TestLLMClientRespectsCustomHTTPClient confirms an injected
// http.Client is used (for tests, proxies, middleware). Asserts by
// wrapping the transport and counting calls.
func TestLLMClientRespectsCustomHTTPClient(t *testing.T) {
	t.Parallel()
	srv := fakeOpenAIServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(openAIResponse{
			Choices: []openAIChoice{{Message: openAIResponseMessage{Content: "ok"}}},
		})
	})
	var callCount int
	hc := &http.Client{Transport: countingRoundTripper{inner: http.DefaultTransport, count: &callCount}}
	client, _ := NewLLMClient(LLMClientConfig{
		Endpoint:   srv.URL,
		Model:      "m",
		HTTPClient: hc,
	})
	_, _ = client.Chat(context.Background(), ChatRequest{})
	if callCount != 1 {
		t.Errorf("custom HTTPClient not used; expected 1 round-trip, got %d", callCount)
	}
}

type countingRoundTripper struct {
	inner http.RoundTripper
	count *int
}

func (c countingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	*c.count++
	return c.inner.RoundTrip(r)
}

// Verify fmt is imported even if its only use is in lint-required test code paths.
var _ = fmt.Sprintf
