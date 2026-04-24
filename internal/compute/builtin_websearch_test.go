package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegisterWebSearchBuiltinRequiresKey(t *testing.T) {
	t.Parallel()
	b := NewBuiltins()
	if err := RegisterWebSearchBuiltin(b, WebSearchConfig{}); err == nil {
		t.Error("missing API key should fail register")
	}
}

func TestWebSearchBuiltinHappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		var req exaSearchRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Query != "golang generics" {
			t.Errorf("query = %q", req.Query)
		}
		if req.NumResults != 3 {
			t.Errorf("numResults = %d; want 3", req.NumResults)
		}
		resp := exaSearchResponse{
			Results: []exaResult{
				{Title: "Go Generics Explained", URL: "https://go.dev/x", Text: strings.Repeat("a", 1000)},
				{Title: "Generics FAQ", URL: "https://go.dev/faq", Text: "short"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	b := NewBuiltins()
	if err := RegisterWebSearchBuiltin(b, WebSearchConfig{
		APIKey:     "test-key",
		Endpoint:   srv.URL,
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
	}); err != nil {
		t.Fatal(err)
	}
	fn, ok := b.Get("web_search")
	if !ok {
		t.Fatal("web_search not registered")
	}
	stdout, exit, err := fn(context.Background(), map[string]string{
		"query":       "golang generics",
		"num_results": "3",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if exit != 0 {
		t.Errorf("exit = %d; want 0", exit)
	}
	var payload struct {
		Query   string      `json:"query"`
		Results []exaResult `json:"results"`
	}
	if err := json.Unmarshal(stdout, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Query != "golang generics" {
		t.Errorf("echoed query = %q", payload.Query)
	}
	if len(payload.Results) != 2 {
		t.Fatalf("results = %d; want 2", len(payload.Results))
	}
	// Long snippet should be truncated with an ellipsis.
	if !strings.HasSuffix(payload.Results[0].Text, "…") {
		t.Errorf("first snippet should be truncated with …; got len=%d", len(payload.Results[0].Text))
	}
}

func TestWebSearchBuiltinRejectsEmptyQuery(t *testing.T) {
	t.Parallel()
	b := NewBuiltins()
	_ = RegisterWebSearchBuiltin(b, WebSearchConfig{APIKey: "x", Endpoint: "http://127.0.0.1:1"})
	fn, _ := b.Get("web_search")
	_, exit, err := fn(context.Background(), map[string]string{})
	if err == nil || exit == 0 {
		t.Error("empty query should fail")
	}
}

func TestWebSearchBuiltinSurfacesHTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	b := NewBuiltins()
	_ = RegisterWebSearchBuiltin(b, WebSearchConfig{APIKey: "x", Endpoint: srv.URL})
	fn, _ := b.Get("web_search")
	_, _, err := fn(context.Background(), map[string]string{"query": "q"})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Errorf("want 429 error; got %v", err)
	}
}

// TestServerToolsMergedIntoRequest — server tools supplied to
// LLMClientConfig appear in the wire-shape tools array alongside
// function tools.
func TestServerToolsMergedIntoRequest(t *testing.T) {
	t.Parallel()
	var captured openAIRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer srv.Close()

	client, err := NewLLMClient(LLMClientConfig{
		Endpoint: srv.URL,
		Model:    "test",
		ServerTools: []ServerTool{
			{Type: "openrouter:web_search", Parameters: map[string]any{"max_results": 5}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(captured.Tools) != 1 {
		t.Fatalf("tools = %d; want 1", len(captured.Tools))
	}
	entry, ok := captured.Tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tool entry not an object: %T", captured.Tools[0])
	}
	if entry["type"] != "openrouter:web_search" {
		t.Errorf("type = %v", entry["type"])
	}
}
