package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// startRESTWithPrompts brings up a Server with a PromptRegistry wired
// and returns the base URL + the registry so tests can both exercise
// the HTTP endpoints and assert on the underlying registry state.
func startRESTWithPrompts(t *testing.T, agent *compute.Agent) (string, *PromptRegistry) {
	t.Helper()
	reg := NewPromptRegistry()
	srv := NewServer(RESTConfig{
		Addr:            "127.0.0.1:0",
		Prompts:         reg,
		ConfirmationTTL: time.Minute,
	}, agent)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = srv.Start(ctx)
	}()
	deadline := time.Now().Add(time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.Addr() == "" {
		cancel()
		wg.Wait()
		t.Fatal("server didn't bind")
	}
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return "http://" + srv.Addr(), reg
}

func TestRESTPromptGetReturnsState(t *testing.T) {
	t.Parallel()
	url, reg := startRESTWithPrompts(t, nil)

	p, err := reg.Create("turn-99", "scary action", "rest", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(url + "/v1/prompts/" + p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET prompt: %d", resp.StatusCode)
	}
	var body promptJSON
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.ID != p.ID || body.TurnID != "turn-99" || body.Reason != "scary action" {
		t.Errorf("response body: %+v", body)
	}
	if body.Decision != "pending" {
		t.Errorf("new prompt should be pending; got %q", body.Decision)
	}
}

func TestRESTPromptGetUnknownIs404(t *testing.T) {
	t.Parallel()
	url, _ := startRESTWithPrompts(t, nil)
	resp, err := http.Get(url + "/v1/prompts/doesnotexist")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown id should 404; got %d", resp.StatusCode)
	}
}

func TestRESTPromptResolveApprove(t *testing.T) {
	t.Parallel()
	url, reg := startRESTWithPrompts(t, nil)
	p, _ := reg.Create("t", "r", "rest", time.Minute)

	resp, err := http.Post(url+"/v1/prompts/"+p.ID+"/resolve",
		"application/json", strings.NewReader(`{"approve":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("resolve status: %d body=%s", resp.StatusCode, raw)
	}
	var body struct {
		Decision string `json:"decision"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Decision != "approved" {
		t.Errorf("decision: %q", body.Decision)
	}
	snap, _ := reg.Get(p.ID)
	if snap.Decision != PromptApproved {
		t.Errorf("registry state: %s", snap.Decision)
	}
}

func TestRESTPromptResolveDeny(t *testing.T) {
	t.Parallel()
	url, reg := startRESTWithPrompts(t, nil)
	p, _ := reg.Create("t", "r", "rest", time.Minute)

	resp, err := http.Post(url+"/v1/prompts/"+p.ID+"/resolve",
		"application/json", strings.NewReader(`{"approve":false}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deny status: %d", resp.StatusCode)
	}
	snap, _ := reg.Get(p.ID)
	if snap.Decision != PromptDenied {
		t.Errorf("registry state after deny: %s", snap.Decision)
	}
}

// TestRESTPromptResolveDoubleIs409 — idempotent-on-conflict: user
// can't flip a decision after it's been recorded.
func TestRESTPromptResolveDoubleIs409(t *testing.T) {
	t.Parallel()
	url, reg := startRESTWithPrompts(t, nil)
	p, _ := reg.Create("t", "r", "rest", time.Minute)

	// First resolve succeeds.
	resp1, _ := http.Post(url+"/v1/prompts/"+p.ID+"/resolve",
		"application/json", strings.NewReader(`{"approve":true}`))
	defer func() { _ = resp1.Body.Close() }()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first resolve: %d", resp1.StatusCode)
	}

	// Second attempt must 409.
	resp2, _ := http.Post(url+"/v1/prompts/"+p.ID+"/resolve",
		"application/json", strings.NewReader(`{"approve":false}`))
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("second resolve should 409; got %d", resp2.StatusCode)
	}
}

func TestRESTPromptResolveUnknownIs404(t *testing.T) {
	t.Parallel()
	url, _ := startRESTWithPrompts(t, nil)
	resp, err := http.Post(url+"/v1/prompts/nonexistent/resolve",
		"application/json", strings.NewReader(`{"approve":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown prompt resolve should 404; got %d", resp.StatusCode)
	}
}

func TestRESTPromptResolveRejectsBadJSON(t *testing.T) {
	t.Parallel()
	url, reg := startRESTWithPrompts(t, nil)
	p, _ := reg.Create("t", "r", "rest", time.Minute)
	resp, err := http.Post(url+"/v1/prompts/"+p.ID+"/resolve",
		"application/json", strings.NewReader("not json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad JSON should 400; got %d", resp.StatusCode)
	}
}

func TestRESTPromptGetWrongMethod(t *testing.T) {
	t.Parallel()
	url, reg := startRESTWithPrompts(t, nil)
	p, _ := reg.Create("t", "r", "rest", time.Minute)
	// POST on the bare id endpoint should 405.
	resp, err := http.Post(url+"/v1/prompts/"+p.ID, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST on GET-only endpoint should 405; got %d", resp.StatusCode)
	}
}

func TestRESTPromptResolveWrongMethod(t *testing.T) {
	t.Parallel()
	url, reg := startRESTWithPrompts(t, nil)
	p, _ := reg.Create("t", "r", "rest", time.Minute)
	resp, err := http.Get(url + "/v1/prompts/" + p.ID + "/resolve")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET on resolve endpoint should 405; got %d", resp.StatusCode)
	}
}

// TestRESTPromptUnmountedWithoutRegistry — without Prompts in config,
// the /v1/prompts/ route is not mounted at all. Confirms we don't
// leak a stub endpoint in minimal deployments.
func TestRESTPromptUnmountedWithoutRegistry(t *testing.T) {
	t.Parallel()
	url, _ := startREST(t, nil) // default config, no Prompts

	resp, err := http.Get(url + "/v1/prompts/whatever")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Default mux returns 404 for unmatched routes.
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unmounted route should 404; got %d", resp.StatusCode)
	}
}

// TestRESTMessageShapeIncludesPromptIDField asserts the JSON shape
// of messageResponse exposes prompt_id so the field survives any
// future refactoring. An end-to-end confirmation trigger through
// the REST server would require a registered tool + a budget that
// trips mid-turn; that path is covered by agent_test.go. This test
// pins the wire format the client relies on.
func TestRESTMessageShapeIncludesPromptIDField(t *testing.T) {
	t.Parallel()
	out := messageResponse{PromptID: "abc123"}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"prompt_id":"abc123"`)) {
		t.Errorf("messageResponse JSON missing prompt_id: %s", raw)
	}
	// And should omit when empty.
	emptyOut := messageResponse{}
	emptyRaw, _ := json.Marshal(emptyOut)
	if bytes.Contains(emptyRaw, []byte(`"prompt_id"`)) {
		t.Errorf("empty PromptID should omit from JSON: %s", emptyRaw)
	}
}
