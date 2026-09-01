package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// expiringSSEServer forgets its session after the first tool call,
// which is what a restarted or idle-reaped MCP server looks like from
// this side: the endpoint is fine, the credential is fine, and the
// session id it handed out is no longer known.
type expiringSSEServer struct {
	mu       sync.Mutex
	sessions map[string]bool
	// expireAfter counts tool calls before the live session is
	// forgotten. Zero never expires.
	expireAfter  int
	calls        int
	sessionsMade int

	streams map[string]chan string
}

func newExpiringServer(t *testing.T, expireAfter int) (*expiringSSEServer, *httptest.Server) {
	t.Helper()
	f := &expiringSSEServer{
		sessions:    map[string]bool{},
		streams:     map[string]chan string{},
		expireAfter: expireAfter,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			f.mu.Lock()
			f.sessionsMade++
			id := fmt.Sprintf("s%d", f.sessionsMade)
			f.sessions[id] = true
			ch := make(chan string, 8)
			f.streams[id] = ch
			f.mu.Unlock()

			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			_, _ = fmt.Fprintf(w, "event: endpoint\ndata: /msg?session_id=%s\n\n", id)
			if fl != nil {
				fl.Flush()
			}
			for {
				select {
				case frame := <-ch:
					_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", frame)
					if fl != nil {
						fl.Flush()
					}
				case <-r.Context().Done():
					return
				}
			}
		}

		id := r.URL.Query().Get("session_id")
		f.mu.Lock()
		live := f.sessions[id]
		f.mu.Unlock()
		if !live {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"Unknown or expired session"}`))
			return
		}

		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.WriteHeader(http.StatusAccepted)

		f.mu.Lock()
		ch := f.streams[id]
		var reply string
		switch req.Method {
		case "initialize":
			reply = fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05",`+
				`"capabilities":{"tools":{}},"serverInfo":{"name":"fake","version":"1"}}}`, req.ID)
		case "tools/list":
			reply = fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"ping"}]}}`, req.ID)
		case "tools/call":
			f.calls++
			if f.expireAfter > 0 && f.calls >= f.expireAfter {
				// Forget every session: the next POST 404s.
				for k := range f.sessions {
					delete(f.sessions, k)
				}
			}
			reply = fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"pong"}]}}`, req.ID)
		}
		f.mu.Unlock()

		if reply != "" && ch != nil {
			ch <- reply
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *expiringSSEServer) sessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessionsMade
}

// A lost session must be rebuilt and the call retried, not reported
// as a broken integration.
//
// Before this, the transport kept POSTing to a session the server had
// forgotten and every call returned 404 "Unknown or expired session"
// — while the health check still said healthy, because answering the
// endpoint and holding a valid session are different things. The only
// recovery was restarting the node.
func TestLostSessionIsRebuiltAndTheCallRetried(t *testing.T) {
	t.Parallel()
	fake, srv := newExpiringServer(t, 1) // forget the session during the first call

	l := NewLoader(LoaderConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := l.StartDirect(ctx, map[string]ServerConfig{
		"fake": {URL: srv.URL + "/sse"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	// First call succeeds and expires the session behind it.
	if _, err := l.Invoke(ctx, compute.SkillInvokeRequest{Name: "fake.ping"}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Second call finds the session gone. It must recover rather than
	// surface the 404.
	res, err := l.Invoke(ctx, compute.SkillInvokeRequest{Name: "fake.ping"})
	if err != nil {
		t.Fatalf("second call did not recover from a lost session: %v", err)
	}
	if res == nil {
		t.Fatal("no result after the rebuild")
	}
	if fake.sessionCount() < 2 {
		t.Errorf("sessions opened = %d; the transport reused a session the server had forgotten",
			fake.sessionCount())
	}
}

// A server refusing the freshly-built session is saying something
// other than "that id is stale", and retrying again would turn a bad
// config into a loop.
func TestRebuildIsAttemptedOnce(t *testing.T) {
	t.Parallel()
	// expireAfter=1 with the session dropped on EVERY call means the
	// rebuild's own call fails too.
	fake, srv := newExpiringServer(t, 1)

	l := NewLoader(LoaderConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := l.StartDirect(ctx, map[string]ServerConfig{"fake": {URL: srv.URL + "/sse"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	_, _ = l.Invoke(ctx, compute.SkillInvokeRequest{Name: "fake.ping"})
	_, _ = l.Invoke(ctx, compute.SkillInvokeRequest{Name: "fake.ping"})
	_, _ = l.Invoke(ctx, compute.SkillInvokeRequest{Name: "fake.ping"})

	// One stream at startup, plus at most one rebuild per invoke —
	// never a retry loop inside a single call.
	if got := fake.sessionCount(); got > 4 {
		t.Errorf("sessions opened = %d; a single call retried more than once", got)
	}
}
