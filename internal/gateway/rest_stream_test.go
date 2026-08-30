package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// REST could not report progress and — the one that actually bites —
// had no hard timeout, so a stalled provider hung the request until
// the client gave up. A request/response API cannot show a typing
// indicator, but it can stream.

// startRESTWith brings up a server with an explicit config, so a test
// can set the timers it is about.
func startRESTWith(t *testing.T, cfg RESTConfig, agent *compute.Agent) string {
	t.Helper()
	cfg.Addr = "127.0.0.1:0"
	srv := NewServer(cfg, agent)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = srv.Start(ctx)
	})
	deadline := time.Now().Add(2 * time.Second)
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
	return "http://" + srv.Addr()
}

// sseEvent is one parsed `event:`/`data:` pair.
type sseEvent struct {
	Name string
	Data map[string]any
}

func readSSE(t *testing.T, body *bufio.Reader) []sseEvent {
	t.Helper()
	var out []sseEvent
	var name string
	for {
		line, err := body.ReadString('\n')
		if err != nil {
			return out
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var payload map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
				t.Fatalf("bad SSE data line %q: %v", line, err)
			}
			out = append(out, sseEvent{Name: name, Data: payload})
		}
	}
}

func postRaw(t *testing.T, url, accept string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/v1/messages",
		strings.NewReader(`{"message":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// A client that asks for a stream gets one, and the reply arrives as
// the closing event rather than as a JSON body.
func TestRESTStreamsWhenTheClientAsks(t *testing.T) {
	t.Parallel()
	url := startRESTWith(t, RESTConfig{}, mockAgent(t, compute.MockResponse{Content: "hi there"}))

	resp := postRaw(t, url, "text/event-stream")
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	events := readSSE(t, bufio.NewReader(resp.Body))
	if len(events) == 0 {
		t.Fatal("no events at all")
	}
	last := events[len(events)-1]
	if last.Name != "final" {
		t.Fatalf("last event = %q, want final: %+v", last.Name, events)
	}
	if last.Data["reply"] != "hi there" {
		t.Errorf("final reply = %v, want the agent's answer", last.Data["reply"])
	}
}

// A client that did not ask must be byte-for-byte unchanged. Streaming
// at anyone who did not opt in would break every existing caller.
func TestRESTStaysJSONWithoutAnAcceptHeader(t *testing.T) {
	t.Parallel()
	url := startRESTWith(t, RESTConfig{}, mockAgent(t, compute.MockResponse{Content: "hi there"}))

	resp := postRaw(t, url, "")
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["reply"] != "hi there" {
		t.Errorf("reply = %v", body["reply"])
	}
}

// A stream must not be silent while the turn runs — that is the whole
// reason to stream. Progress has to arrive before the reply.
//
// Needs a turn slow enough to be worth narrating: against an instant
// mock the handler can finish before the first typing tick, which is
// correct behaviour (a fast turn says nothing) and proves nothing.
func TestRESTStreamsProgressBeforeTheReply(t *testing.T) {
	t.Parallel()
	url := startRESTWith(t, RESTConfig{
		TypingInterval: 5 * time.Millisecond,
		InterimTimeout: 10 * time.Millisecond,
	}, slowAgent(t, 120*time.Millisecond))

	resp := postRaw(t, url, "text/event-stream")
	events := readSSE(t, bufio.NewReader(resp.Body))

	var sawTyping, sawInterim bool
	for _, e := range events {
		switch e.Name {
		case "typing":
			sawTyping = true
		case "interim":
			sawInterim = true
		case "final":
			if !sawTyping {
				t.Errorf("the reply arrived with no typing signal before it: %+v", events)
			}
			if !sawInterim {
				t.Errorf("a slow turn sent no progress message: %+v", events)
			}
			return
		}
	}
	t.Errorf("no final event: %+v", events)
}

// slowAgent answers, but not before the responsiveness timers have
// something to say.
func slowAgent(t *testing.T, delay time.Duration) *compute.Agent {
	t.Helper()
	agent, err := compute.NewAgent(compute.AgentConfig{
		Provider: slowProvider{delay: delay},
	})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

type slowProvider struct{ delay time.Duration }

func (p slowProvider) Chat(ctx context.Context, _ compute.ChatRequest) (*compute.ChatResponse, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(p.delay):
		return &compute.ChatResponse{Content: "done"}, nil
	}
}

// The gap this closes even for non-streaming clients. Without a cap a
// stalled provider hangs the request until the caller gives up.
func TestRESTHardTimeoutEndsAStalledTurn(t *testing.T) {
	t.Parallel()
	url := startRESTWith(t, RESTConfig{
		TypingInterval: -1,
		InterimTimeout: -1,
		HardTimeout:    30 * time.Millisecond,
	}, stallingAgent(t))

	start := time.Now()
	resp := postRaw(t, url, "")
	elapsed := time.Since(start)

	// Terminating at all is the point. Before this, the hard timeout
	// cancelled the first provider call and the graceful-reply path
	// re-entered the same stalled provider on context.Background() —
	// so the request hung until the client gave up, and the cap the
	// gateway set did nothing.
	if elapsed > 5*time.Second {
		t.Fatalf("request took %v; the turn never terminated", elapsed)
	}

	// 200 with an honest "this took too long" is the right answer, not
	// a 500: the user gets told what happened rather than shown a
	// stack-trace-shaped error.
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	reply, _ := body["reply"].(string)
	if !strings.Contains(strings.ToLower(reply), "too long") {
		t.Errorf("reply = %q; it does not tell the user the turn was cut short", reply)
	}
	// And specifically not the tool-limit wording, which would send
	// them off to narrow a request that was never too broad.
	if strings.Contains(strings.ToLower(reply), "tool-call limit") {
		t.Errorf("a timed-out turn blamed the tool-call limit: %q", reply)
	}
}

// stallingAgent never answers, standing in for a provider that has
// stopped responding without closing the connection.
func stallingAgent(t *testing.T) *compute.Agent {
	t.Helper()
	agent, err := compute.NewAgent(compute.AgentConfig{
		Provider: stallingProvider{},
		// The graceful "I ran out of time" reply re-enters the
		// provider on a fresh context, so it needs its own bound.
		// Short here to keep the test quick; 15s in production.
		SummaryTimeout: 30 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

// stallingProvider blocks until the turn's context ends.
type stallingProvider struct{}

func (stallingProvider) Chat(ctx context.Context, _ compute.ChatRequest) (*compute.ChatResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
