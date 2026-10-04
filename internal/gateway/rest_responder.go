package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// REST had none of the responsiveness work: no typing signal, no
// interim progress, and — the one that actually bites — no hard
// timeout. A stalled provider hung the request until the client gave
// up, where Telegram has capped a turn at 90s since it shipped.
//
// A request/response API cannot show a typing indicator, but it can
// stream, and a client that asks for SSE gets the same three signals
// Telegram does. One that does not is unchanged: the timers still run,
// so the hard timeout applies either way, and the progress calls go
// nowhere rather than corrupting a JSON body.

// restResponder writes progress events to an SSE stream, or nothing
// at all when the client did not ask for one.
type restResponder struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	// streaming is false for an ordinary JSON client. Every method
	// then returns nil without writing — the handler owns the body.
	streaming bool
	// closed guards against a timer goroutine writing after the
	// handler has begun the final response. Without it a slow turn
	// that finishes just as the interim timer fires would interleave
	// an event into the JSON body.
	closed bool
}

// newRESTResponder decides whether this request can be streamed.
//
// Requires BOTH the client asking and the writer supporting Flush.
// Buffering an SSE stream defeats the point entirely — the client
// would receive every "progress" event at once, after the turn it was
// meant to narrate had already finished.
func newRESTResponder(w http.ResponseWriter, r *http.Request) *restResponder {
	resp := &restResponder{w: w}
	flusher, canFlush := w.(http.Flusher)
	if !canFlush || !acceptsEventStream(r) {
		return resp
	}
	resp.flusher = flusher
	resp.streaming = true

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Proxies that buffer by default turn a stream into a very slow
	// single response; this is the header nginx reads.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return resp
}

// Streaming reports whether the handler should emit its final payload
// as an event rather than as a JSON body.
func (r *restResponder) Streaming() bool {
	if r == nil {
		return false
	}
	return r.streaming
}

func (r *restResponder) Typing(context.Context) error {
	return r.event("typing", map[string]any{})
}

func (r *restResponder) Interim(_ context.Context, text string) error {
	return r.event("interim", map[string]any{"text": text})
}

func (r *restResponder) Final(_ context.Context, text string) error {
	return r.event("final", map[string]any{"reply": text})
}

// sendFinal emits the turn's full payload as the closing event. The
// REST response carries tool calls and budget state as well as the
// reply, so Final's text-only shape is not enough for it — Final
// exists for the shared timer path, which only ever has text.
//
// Bypasses the closed guard on purpose: Close is called first
// precisely so the timers stop, and this is the write it stopped them
// for.
func (r *restResponder) sendFinal(payload any) error {
	return r.forceEvent("final", payload)
}

// sendError reports a failure mid-stream. Headers are already out by
// then, so an HTTP status cannot carry it and a client would otherwise
// wait on a connection that never explains itself.
func (r *restResponder) sendError(msg string) error {
	return r.forceEvent("error", map[string]any{"error": msg})
}

// Close stops any further events. Called before the handler writes its
// response, so a timer that fires at exactly the wrong moment cannot
// interleave into the body.
func (r *restResponder) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
}

// event writes a progress event, or nothing once the turn has begun
// its final write.
//
// THE CLOSED CHECK AND THE WRITE HAPPEN UNDER ONE LOCK.
//
// They used to be two: the flag was read, the lock released, and
// forceEvent then took it again to write. A typing keepalive firing in
// that gap passed a check that was already stale and wrote to a
// ResponseWriter whose handler had returned — which panics, and a
// panic in a timer goroutine takes the whole process with it. In tests
// that showed up as internal/gateway failing perhaps one run in four,
// with a stack in typingKeepalive and nothing wrong at the line named.
//
// The flag was always meant to prevent exactly this; check-then-act
// meant it could not.
func (r *restResponder) event(name string, payload any) error {
	if r == nil || !r.streaming {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	return r.writeEventLocked(name, body)
}

// forceEvent writes regardless of closed. The handler's own final and
// error events use it: Close is called BEFORE the handler writes, to
// shut the timers up, so the write it is shutting them up for must not
// be blocked by the same flag.
func (r *restResponder) forceEvent(name string, payload any) error {
	if r == nil || !r.streaming {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writeEventLocked(name, body)
}

// writeEventLocked emits one SSE frame. Caller holds r.mu.
func (r *restResponder) writeEventLocked(name string, body []byte) error {
	if _, err := r.w.Write([]byte("event: " + name + "\ndata: ")); err != nil {
		return err
	}
	if _, err := r.w.Write(body); err != nil {
		return err
	}
	if _, err := r.w.Write([]byte("\n\n")); err != nil {
		return err
	}
	r.flusher.Flush()
	return nil
}

// acceptsEventStream reads the Accept header. Matched on substring
// rather than parsed, because a client sending
// "text/event-stream, application/json" means it can take either and
// the stream is the better one to give it.
func acceptsEventStream(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream")
}
