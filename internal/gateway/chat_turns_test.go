package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type reconnectRunner struct {
	calls                       atomic.Int32
	started, release, cancelled chan struct{}
}

func (r *reconnectRunner) Run(ctx context.Context, request turn.Request) (*turn.Response, error) {
	r.calls.Add(1)
	select {
	case r.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		close(r.cancelled)
		return nil, ctx.Err()
	case <-r.release:
		return &turn.Response{Reply: "Hello after reconnect", Messages: []turn.Message{{Role: "user", Content: request.Message}, {Role: "assistant", Content: "Hello after reconnect"}}}, nil
	}
}
func (r *reconnectRunner) Resume(ctx context.Context, request turn.Request, _ []turn.Message) (*turn.Response, error) {
	return r.Run(ctx, request)
}

func readChatTurn(t *testing.T, response *http.Response) chatTurn {
	t.Helper()
	defer func() { _ = response.Body.Close() }()
	var result chatTurn
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}
func awaitChatTurn(t *testing.T, server *Server, id, state string, auth http.Header) chatTurn {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		response := doJSON(t, http.MethodGet, webBaseURL(server)+"/v1/chat-turns/"+id, "", auth)
		if response.StatusCode != 200 {
			t.Fatal(response.StatusCode)
		}
		result := readChatTurn(t, response)
		if result.State == state {
			return result
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("turn did not reach %s", state)
	return chatTurn{}
}

func TestRetainedChatDisconnectIdempotencyOwnershipAndCancellation(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "remote"}[remote], func(t *testing.T) {
			runner := &reconnectRunner{started: make(chan struct{}, 1), release: make(chan struct{}), cancelled: make(chan struct{})}
			backend := startWebREST(t, runner, func(c *RESTConfig) {
				c.Bots = stubBots{rec: &pb.BotRecord{Id: "chief", Owner: "user:alice", Enabled: true}}
			})
			server := backend
			if remote {
				client := testConsoleClient(t, backend)
				server = startWebREST(t, nil, func(c *RESTConfig) { c.RemoteConsole = client })
			}
			auth := http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}}
			body := `{"id":"reconnect-test","bot":"chief","message":"hello"}`
			response := doJSON(t, http.MethodPost, webBaseURL(server)+"/v1/chat-turns", body, auth)
			if response.StatusCode != 202 {
				t.Fatal(response.StatusCode)
			}
			_ = response.Body.Close() // The entire HTTP request is gone while execution continues.
			select {
			case <-runner.started:
			case <-time.After(time.Second):
				t.Fatal("not started")
			}
			retry := doJSON(t, http.MethodPost, webBaseURL(server)+"/v1/chat-turns", body, auth)
			if retry.StatusCode != 202 {
				t.Fatal(retry.StatusCode)
			}
			_ = retry.Body.Close()
			if runner.calls.Load() != 1 {
				t.Fatal("duplicate execution")
			}
			foreign := http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "bob", nil)}}
			for _, method := range []string{http.MethodGet, http.MethodDelete} {
				result := doJSON(t, method, webBaseURL(server)+"/v1/chat-turns/reconnect-test", "", foreign)
				if result.StatusCode != 404 {
					t.Fatalf("foreign %s returned %d", method, result.StatusCode)
				}
				_ = result.Body.Close()
			}
			close(runner.release)
			result := awaitChatTurn(t, server, "reconnect-test", "completed", auth)
			if !strings.Contains(string(result.Data), "Hello after reconnect") {
				t.Fatal(string(result.Data))
			}
			if runner.calls.Load() != 1 {
				t.Fatal("reconnect ran again")
			}
		})
	}
}

func TestRetainedChatExplicitStop(t *testing.T) {
	runner := &reconnectRunner{started: make(chan struct{}, 1), release: make(chan struct{}), cancelled: make(chan struct{})}
	server := startWebREST(t, runner, func(c *RESTConfig) {
		c.Bots = stubBots{rec: &pb.BotRecord{Id: "chief", Owner: "user:alice", Enabled: true}}
	})
	auth := http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}}
	response := doJSON(t, http.MethodPost, webBaseURL(server)+"/v1/chat-turns", `{"id":"stop-test","bot":"chief","message":"hello"}`, auth)
	_ = response.Body.Close()
	<-runner.started
	response = doJSON(t, http.MethodDelete, webBaseURL(server)+"/v1/chat-turns/stop-test", "", auth)
	if readChatTurn(t, response).State != "cancelled" {
		t.Fatal("stop did not retain cancellation")
	}
	select {
	case <-runner.cancelled:
	case <-time.After(time.Second):
		t.Fatal("runner not cancelled")
	}
}

func TestRetainedChatPendingConfirmationSurvivesReconnect(t *testing.T) {
	runner := &approvalConsoleRunner{resumed: make(chan turn.Request, 1)}
	server := startWebREST(t, runner, func(c *RESTConfig) {
		c.Bots = stubBots{rec: &pb.BotRecord{Id: "chief", Owner: "user:alice", Enabled: true}}
	})
	auth := http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}}
	response := doJSON(t, http.MethodPost, webBaseURL(server)+"/v1/chat-turns", `{"id":"approval","bot":"chief","message":"write"}`, auth)
	_ = response.Body.Close()
	waiting := awaitChatTurn(t, server, "approval", "waiting", auth)
	var prompt struct {
		ID string `json:"promptId"`
	}
	if err := json.Unmarshal(waiting.Data, &prompt); err != nil || prompt.ID == "" {
		t.Fatal("missing pending confirmation")
	}
	// A heartbeat cannot replace the prompt needed by the reopened browser.
	if err := server.chatTurns.event("alice", "approval", "working", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if got := server.chatTurns.get("alice", "approval"); got.Event != "needs_confirmation" {
		t.Fatal("heartbeat erased pending question")
	}
	response = doJSON(t, http.MethodPost, webBaseURL(server)+"/v1/prompts/"+prompt.ID+"/resolve", `{"approve":true}`, auth)
	if response.StatusCode != 200 {
		t.Fatal(response.StatusCode)
	}
	_ = response.Body.Close()
	awaitChatTurn(t, server, "approval", "completed", auth)
}

func TestRetainedChatLogoutStopsServerOwnedExecution(t *testing.T) {
	runner := &reconnectRunner{started: make(chan struct{}, 1), release: make(chan struct{}), cancelled: make(chan struct{})}
	server := startWebREST(t, runner, func(c *RESTConfig) {
		c.Bots = stubBots{rec: &pb.BotRecord{Id: "chief", Owner: "user:alice", Enabled: true}}
	})
	if err := server.logins.put(&loginSession{ID: "browser", UserID: "alice", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	auth := http.Header{"Cookie": {LoginCookieName + "=browser"}, "Origin": {webBaseURL(server)}}
	response := doJSON(t, http.MethodPost, webBaseURL(server)+"/v1/chat-turns", `{"id":"logout","bot":"chief","message":"hello"}`, auth)
	if response.StatusCode != 202 {
		t.Fatal(response.StatusCode)
	}
	_ = response.Body.Close()
	<-runner.started
	response = doJSON(t, http.MethodDelete, webBaseURL(server)+"/v1/session", "", auth)
	_ = response.Body.Close()
	select {
	case <-runner.cancelled:
	case <-time.After(time.Second):
		t.Fatal("logout left execution running")
	}
}

func TestChatJournalEncryptsResultsAndMarksInterruptedWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth", "browser-sessions.json")
	journal := newChatTurns()
	if err := journal.open(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"done", "running"} {
		_, created, err := journal.create("alice", chatTurnRequest{ID: id, Bot: id, Message: "private greeting"}, func() {})
		if err != nil || !created {
			t.Fatal(err)
		}
		journal.wg.Done()
	}
	if err := journal.event("alice", "done", "reply", json.RawMessage(`{"text":"private result"}`)); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(journal.dir, chatTurnKey("alice", "done")+".turn")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private") {
		t.Fatal("plaintext transcript on disk")
	}
	info, _ := os.Stat(file)
	if info.Mode().Perm() != 0o600 {
		t.Fatal("journal file not private")
	}
	reopened := newChatTurns()
	if err := reopened.open(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	if got := reopened.get("alice", "done"); got == nil || got.State != "completed" || !strings.Contains(string(got.Data), "private result") {
		t.Fatal("completed result lost")
	}
	if got := reopened.get("alice", "running"); got == nil || got.State != "interrupted" {
		t.Fatal("unfinished turn was replayable")
	}
	if reopened.get("bob", "done") != nil {
		t.Fatal("foreign result leaked")
	}
}

func TestChatJournalCreationFailureDoesNotAdmitWork(t *testing.T) {
	journal := newChatTurns()
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := journal.open(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(journal.dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal.dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, created, err := journal.create("alice", chatTurnRequest{ID: "fail", Message: "test"}, func() {})
	if err == nil || created || len(journal.entries) != 0 {
		t.Fatal("failed persistence admitted work")
	}
}
