package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// socketLoop had no test coverage at all, and the first change made to
// it shipped a regression: a read deadline that could not tell a quiet
// socket from a dead one, so it tore down healthy connections every
// sixty seconds. These two cases pin the distinction.

// fakeSocketMode serves both halves a connection needs: the
// apps.connections.open call that hands out the socket URL, and the
// socket itself.
//
// answerPings decides which kind of peer it is. coder/websocket replies
// to pings from inside Read, so a server that reads is alive and a
// server that never reads is indistinguishable from a partition —
// which is exactly the pair under test.
func fakeSocketMode(t *testing.T, answerPings bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server

	mux.HandleFunc("/apps.connections.open", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"ok":  true,
			"url": "ws" + strings.TrimPrefix(srv.URL, "http") + "/socket",
		})
	})
	mux.HandleFunc("/socket", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		if !answerPings {
			// Accepted and then ignored. Never reading means never
			// answering a ping, which is what a dead peer looks like.
			<-r.Context().Done()
			return
		}
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
		}
	})

	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func socketTestHandler(t *testing.T, srv *httptest.Server) *SlackHandler {
	t.Helper()
	return &SlackHandler{
		cfg:            SlackConfig{BotToken: "xoxb-test", AppToken: "xapp-test"},
		log:            discardLogger(),
		api:            newSlackAPI("xoxb-test", srv.URL, srv.Client()),
		keepaliveEvery: 20 * time.Millisecond,
		pongWithin:     200 * time.Millisecond,
	}
}

// The regression. A Socket Mode connection with nothing happening on it
// is legitimately silent for minutes; it must survive many keepalive
// cycles without a single envelope arriving.
func TestSlackQuietSocketSurvives(t *testing.T) {
	t.Parallel()

	h := socketTestHandler(t, fakeSocketMode(t, true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- h.runOneConnection(ctx) }()

	// Many keepalive intervals, no traffic. The old read deadline
	// failed here; a ping/pong keepalive does not, because the pong is
	// a round trip the server answers from inside its own Read.
	select {
	case err := <-done:
		t.Fatalf("a healthy but idle socket was torn down: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	// And it stops when told to, rather than leaking the keepalive
	// goroutine past the connection it belongs to.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("runOneConnection did not return after its context was cancelled")
	}
}

// The other half: a peer that has stopped answering must be noticed,
// which is the concern the read deadline was reaching for.
func TestSlackDeadPeerIsDetected(t *testing.T) {
	t.Parallel()

	h := socketTestHandler(t, fakeSocketMode(t, false))
	ctx := t.Context()

	done := make(chan error, 1)
	go func() { done <- h.runOneConnection(ctx) }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a dead peer should surface as an error so the caller reconnects")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a peer that never answers a ping was never detected")
	}
}
