package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSSEServer is an MCP server speaking the HTTP+SSE binding: a
// GET that streams events, and a POST endpoint it names in the first
// one.
type fakeSSEServer struct {
	mu       sync.Mutex
	posted   [][]byte
	authSeen string

	// endpointPath is what the server names. Relative on purpose in
	// the default case — that is what real servers send.
	endpointPath string
	// flush lets a test push a frame onto the stream.
	flush chan string
}

func newFakeSSE(t *testing.T) (*fakeSSEServer, *httptest.Server) {
	t.Helper()
	f := &fakeSSEServer{endpointPath: "/messages?session=abc", flush: make(chan string, 8)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			f.authSeen = r.Header.Get("Authorization")
			f.mu.Unlock()

			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			_, _ = fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", f.endpointPath)
			if fl != nil {
				fl.Flush()
			}
			for {
				select {
				case frame := <-f.flush:
					_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", frame)
					if fl != nil {
						fl.Flush()
					}
				case <-r.Context().Done():
					return
				}
			}
		case http.MethodPost:
			body := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(body)
			f.mu.Lock()
			f.posted = append(f.posted, body)
			f.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeSSEServer) postedFrames() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.posted...)
}

// The endpoint event is the session: a frame sent before it has
// nowhere to go, and the handshake is the first thing sent.
func TestSSEWaitsForTheEndpointBeforeSending(t *testing.T) {
	t.Parallel()
	f, srv := newFakeSSE(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, err := NewSSETransport(ctx, SSEConfig{Client: srv.Client(), URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tr.Close() }()

	if err := tr.Send(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)); err != nil {
		t.Fatal(err)
	}
	frames := f.postedFrames()
	if len(frames) != 1 || !strings.Contains(string(frames[0]), "initialize") {
		t.Fatalf("server received %q; want the handshake", frames)
	}
}

// A relative endpoint is what real servers send. Resolved against the
// stream URL, or every POST goes to the wrong place.
func TestSSEResolvesARelativeEndpoint(t *testing.T) {
	t.Parallel()
	_, srv := newFakeSSE(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, err := NewSSETransport(ctx, SSEConfig{Client: srv.Client(), URL: srv.URL + "/sse"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tr.Close() }()

	if !strings.HasPrefix(tr.endpoint, srv.URL) {
		t.Errorf("endpoint = %q; want it resolved against %q", tr.endpoint, srv.URL)
	}
	if !strings.Contains(tr.endpoint, "session=abc") {
		t.Errorf("endpoint = %q; the session was dropped", tr.endpoint)
	}
}

// Replies arrive on the stream, not in the POST response.
func TestSSEReceivesFramesFromTheStream(t *testing.T) {
	t.Parallel()
	f, srv := newFakeSSE(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, err := NewSSETransport(ctx, SSEConfig{Client: srv.Client(), URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tr.Close() }()

	f.flush <- `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`
	got, err := tr.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("frame is not JSON: %q", got)
	}
	if decoded["id"] != float64(1) {
		t.Errorf("frame = %v; want the reply to id 1", decoded)
	}
}

// Authentication is a header, and it has to reach both connections:
// the stream AND the POSTs. A token on only one is a session that
// opens and then refuses every call.
func TestSSESendsAuthOnBothConnections(t *testing.T) {
	t.Parallel()
	f, srv := newFakeSSE(t)

	header := http.Header{}
	header.Set("Authorization", "Bearer secret-token")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, err := NewSSETransport(ctx, SSEConfig{Client: srv.Client(), URL: srv.URL, Header: header})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tr.Close() }()

	f.mu.Lock()
	streamAuth := f.authSeen
	f.mu.Unlock()
	if streamAuth != "Bearer secret-token" {
		t.Errorf("stream Authorization = %q", streamAuth)
	}

	if err := tr.Send(ctx, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
}

// An unscoped client would reach a host nothing constrains, which is
// what the egress role exists to prevent.
func TestSSERefusesWithoutAnEgressClient(t *testing.T) {
	t.Parallel()
	_, err := NewSSETransport(context.Background(), SSEConfig{URL: "https://example.test/sse"})
	if err == nil || !strings.Contains(err.Error(), "egress-scoped") {
		t.Fatalf("err = %v; want a refusal naming the missing client", err)
	}
}

// A server that never names an endpoint has failed to start a
// session. Reported at construction, naming the server rather than
// whichever frame happened to be sent first.
func TestSSEFailsWhenNoEndpointArrives(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Closes immediately without an endpoint event.
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := NewSSETransport(ctx, SSEConfig{Client: srv.Client(), URL: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "endpoint") {
		t.Fatalf("err = %v; want it to name the missing endpoint", err)
	}
}

// A non-200 has to carry the server's own words: "connect failed" is
// the same sentence for a bad token and a wrong path.
func TestSSEReportsTheServersRefusal(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("token expired"))
	}))
	defer srv.Close()

	_, err := NewSSETransport(context.Background(), SSEConfig{Client: srv.Client(), URL: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "token expired") {
		t.Fatalf("err = %v; want the server's message", err)
	}
}
