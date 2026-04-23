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

	jwtlib "github.com/golang-jwt/jwt/v5"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/auth"
)

// startREST spins up a Server bound to a random port and runs it
// in a background goroutine. Returns the base URL and a cancel
// function for cleanup.
func startREST(t *testing.T, agent *compute.Agent) (string, func()) {
	t.Helper()
	srv := NewServer(RESTConfig{Addr: "127.0.0.1:0"}, agent)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = srv.Start(ctx)
	}()
	// Wait briefly for Start to bind.
	deadline := time.Now().Add(time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.Addr() == "" {
		cancel()
		wg.Wait()
		t.Fatal("server didn't bind within 1s")
	}
	url := "http://" + srv.Addr()
	cleanup := func() {
		cancel()
		wg.Wait()
	}
	t.Cleanup(cleanup)
	return url, cleanup
}

// mockAgent is an AgentConfig ready to construct a real compute.Agent
// backed by a MockProvider with scripted responses — used by tests
// that want the HTTP layer exercised through a real agent loop.
func mockAgent(t *testing.T, responses ...compute.MockResponse) *compute.Agent {
	t.Helper()
	provider := compute.NewMockProvider(responses...)
	agent, err := compute.NewAgent(compute.AgentConfig{
		Provider: provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func TestRESTHealthzAlwaysOK(t *testing.T) {
	t.Parallel()
	url, _ := startREST(t, nil) // agent nil is fine for healthz

	resp, err := http.Get(url + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status: %d", resp.StatusCode)
	}
}

func TestRESTReadyzReflectsAgentConfigured(t *testing.T) {
	t.Parallel()
	// No agent → 503.
	url, _ := startREST(t, nil)
	resp, _ := http.Get(url + "/readyz")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("without agent, readyz should be 503; got %d", resp.StatusCode)
	}

	// With agent → 200.
	agent := mockAgent(t, compute.MockResponse{Content: "hi"})
	url2, _ := startREST(t, agent)
	resp2, _ := http.Get(url2 + "/readyz")
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("with agent, readyz should be 200; got %d", resp2.StatusCode)
	}
}

func TestRESTMessagesNoAgent503(t *testing.T) {
	t.Parallel()
	url, _ := startREST(t, nil)
	body := strings.NewReader(`{"message":"hi"}`)
	resp, err := http.Post(url+"/v1/messages", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no agent → 503; got %d", resp.StatusCode)
	}
}

func TestRESTMessagesHappyPath(t *testing.T) {
	t.Parallel()
	agent := mockAgent(t, compute.MockResponse{Content: "42 — a fine answer"})
	url, _ := startREST(t, agent)

	body := bytes.NewBufferString(`{"message":"what is the meaning of life?"}`)
	resp, err := http.Post(url+"/v1/messages", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: %d body=%s", resp.StatusCode, raw)
	}
	var out messageResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Reply, "42") {
		t.Errorf("reply: %q", out.Reply)
	}
	if out.NeedsConfirmation {
		t.Error("no confirmation expected")
	}
}

func TestRESTMessagesRequiresPOST(t *testing.T) {
	t.Parallel()
	agent := mockAgent(t, compute.MockResponse{Content: "ok"})
	url, _ := startREST(t, agent)
	resp, err := http.Get(url + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET should be 405; got %d", resp.StatusCode)
	}
}

func TestRESTMessagesRejectsBadJSON(t *testing.T) {
	t.Parallel()
	agent := mockAgent(t, compute.MockResponse{Content: "ok"})
	url, _ := startREST(t, agent)
	resp, err := http.Post(url+"/v1/messages", "application/json", strings.NewReader("not json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400; got %d", resp.StatusCode)
	}
}

func TestRESTMessagesRejectsEmptyMessage(t *testing.T) {
	t.Parallel()
	agent := mockAgent(t, compute.MockResponse{Content: "ok"})
	url, _ := startREST(t, agent)
	resp, err := http.Post(url+"/v1/messages", "application/json", strings.NewReader(`{"message":""}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty message should 400; got %d", resp.StatusCode)
	}
}

// --- JWT integration ---------------------------------------------------

const restTestSecret = "rest-jwt-test-secret-at-least-32-bytes"

// startRESTWithAuth spins up a Server with a pre-configured
// JWT validator + RequireAuth=true. Used by auth-specific tests.
func startRESTWithAuth(t *testing.T, agent *compute.Agent, requireAuth bool) (string, func()) {
	t.Helper()
	validator, err := auth.NewValidator(auth.Config{
		AllowHS256:  true,
		HS256Secret: restTestSecret,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(RESTConfig{
		Addr:         "127.0.0.1:0",
		JWTValidator: validator,
		RequireAuth:  requireAuth,
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
	cleanup := func() {
		cancel()
		wg.Wait()
	}
	t.Cleanup(cleanup)
	return "http://" + srv.Addr(), cleanup
}

// mintValidJWT produces an HS256 token with the test secret + the
// given scope + a future expiry. Drop-in for Authorization headers.
func mintValidJWT(t *testing.T, scope string) string {
	t.Helper()
	tok := jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, jwtlib.MapClaims{
		"sub":   "test-user",
		"scope": scope,
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	s, err := tok.SignedString([]byte(restTestSecret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRESTRequireAuthRejectsUnauthenticated(t *testing.T) {
	t.Parallel()
	agent := mockAgent(t, compute.MockResponse{Content: "hi"})
	url, _ := startRESTWithAuth(t, agent, true)

	resp, err := http.Post(url+"/v1/messages", "application/json", strings.NewReader(`{"message":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("RequireAuth + no token should 401; got %d", resp.StatusCode)
	}
}

func TestRESTRequireAuthAcceptsValidJWT(t *testing.T) {
	t.Parallel()
	agent := mockAgent(t, compute.MockResponse{Content: "authenticated!"})
	url, _ := startRESTWithAuth(t, agent, true)

	req, _ := http.NewRequest(http.MethodPost, url+"/v1/messages",
		strings.NewReader(`{"message":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+mintValidJWT(t, "operator"))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("valid JWT should allow; got %d body=%s", resp.StatusCode, raw)
	}
	var out messageResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Reply != "authenticated!" {
		t.Errorf("reply: %q", out.Reply)
	}
}

func TestRESTRequireAuthRejectsInvalidJWT(t *testing.T) {
	t.Parallel()
	agent := mockAgent(t, compute.MockResponse{Content: "hi"})
	url, _ := startRESTWithAuth(t, agent, true)

	// Forge a token with the WRONG secret.
	tok := jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, jwtlib.MapClaims{
		"sub": "attacker",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	bad, _ := tok.SignedString([]byte("wrong-secret-obviously-not-right"))

	req, _ := http.NewRequest(http.MethodPost, url+"/v1/messages",
		strings.NewReader(`{"message":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bad)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("SECURITY: bad-secret token should 401; got %d", resp.StatusCode)
	}
}

// TestRESTOptionalAuthFallsBackToAnon — when RequireAuth=false and
// no/bad token is supplied, the request STILL succeeds with the
// default scope. Supports local dev + reverse-proxy-terminated
// deployments where the channel front is already trusted.
func TestRESTOptionalAuthFallsBackToAnon(t *testing.T) {
	t.Parallel()
	agent := mockAgent(t, compute.MockResponse{Content: "anon ok"})
	url, _ := startRESTWithAuth(t, agent, false) // RequireAuth=false

	resp, err := http.Post(url+"/v1/messages", "application/json", strings.NewReader(`{"message":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("RequireAuth=false should allow anon; got %d", resp.StatusCode)
	}
}

// TestRESTRequireAuthNoValidatorIs401 — a deployment that sets
// RequireAuth=true but forgets to configure a JWT validator must
// fail-closed (every request 401), NOT fail-open.
func TestRESTRequireAuthNoValidatorIs401(t *testing.T) {
	t.Parallel()
	agent := mockAgent(t, compute.MockResponse{Content: "ignored"})
	srv := NewServer(RESTConfig{
		Addr:         "127.0.0.1:0",
		RequireAuth:  true,
		JWTValidator: nil, // oops
	}, agent)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Start(ctx) }()
	for srv.Addr() == "" {
		time.Sleep(5 * time.Millisecond)
	}

	resp, err := http.Post("http://"+srv.Addr()+"/v1/messages", "application/json",
		strings.NewReader(`{"message":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("SECURITY: require_auth+nil_validator must reject; got %d", resp.StatusCode)
	}
}

func TestRESTAddrExposedAfterStart(t *testing.T) {
	t.Parallel()
	srv := NewServer(RESTConfig{Addr: "127.0.0.1:0"}, nil)
	if srv.Addr() != "" {
		t.Errorf("Addr before Start should be empty; got %q", srv.Addr())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = srv.Start(ctx)
		close(done)
	}()
	// Wait for bind.
	deadline := time.Now().Add(time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.Addr() == "" {
		cancel()
		<-done
		t.Fatal("server didn't bind within 1s")
	}
	if !strings.HasPrefix(srv.Addr(), "127.0.0.1:") {
		t.Errorf("Addr format: %q", srv.Addr())
	}
	cancel()
	<-done
}
