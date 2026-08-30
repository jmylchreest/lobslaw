package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
