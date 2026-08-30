package gateway

import (
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
)

// The closed flag exists to stop a timer goroutine writing after the
// handler has begun its final response. It could not, because the
// check and the write took the lock separately: a keepalive firing in
// the gap passed a check that was already stale and wrote to a
// ResponseWriter whose handler had returned.
//
// That panics, and a panic in a timer goroutine takes the process with
// it — which showed up as internal/gateway failing about one run in
// four, with a stack in typingKeepalive and nothing wrong at the line
// it named.

// countingWriter records writes and can assert none arrive after a
// point. It stands in for the real ResponseWriter, which panics rather
// than counting when written to late.
type countingWriter struct {
	*httptest.ResponseRecorder
	mu           sync.Mutex
	sealed       bool
	wroteAfter   int
	totalWritten int
}

func (w *countingWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	if w.sealed {
		w.wroteAfter++
	}
	w.totalWritten++
	w.mu.Unlock()
	return w.ResponseRecorder.Write(b)
}

func (w *countingWriter) seal() {
	w.mu.Lock()
	w.sealed = true
	w.mu.Unlock()
}

func (w *countingWriter) lateWrites() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.wroteAfter
}

// Close must mean CLOSED — not "closed, unless a writer is already
// past the check".
//
// Deterministic where the original bug was not: the writer seals
// itself the instant Close returns, and any event that lands after
// that is one the flag was supposed to have stopped. Under the old
// check-then-act this fails; the goroutines are only here to make the
// window wide enough to hit reliably.
func TestNoEventIsWrittenAfterClose(t *testing.T) {
	t.Parallel()
	for attempt := range 400 {
		w := &countingWriter{ResponseRecorder: httptest.NewRecorder()}
		r := &restResponder{w: w, flusher: nopFlusher{}, streaming: true}

		// Several emitters, so one is reliably mid-call when Close
		// lands. A single keepalive is what the real bug had, and a
		// single one is also what made it show up once in four runs
		// rather than every time.
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				<-start
				for range 200 {
					_ = r.Typing(t.Context())
					runtime.Gosched()
				}
			})
		}
		close(start)
		runtime.Gosched()

		r.Close()
		w.seal()
		wg.Wait()

		if n := w.lateWrites(); n != 0 {
			t.Fatalf("attempt %d: %d writes landed after Close", attempt, n)
		}
	}
}

// The handler's own final write must still go out. Close is called
// BEFORE it, precisely to silence the timers, so blocking it on the
// same flag would silence the response as well.
func TestTheFinalEventStillWritesAfterClose(t *testing.T) {
	t.Parallel()
	w := &countingWriter{ResponseRecorder: httptest.NewRecorder()}
	r := &restResponder{w: w, flusher: nopFlusher{}, streaming: true}

	r.Close()
	if err := r.forceEvent("final", map[string]any{"reply": "done"}); err != nil {
		t.Fatal(err)
	}
	if w.totalWritten == 0 {
		t.Fatal("the final event was suppressed by Close")
	}
	if body := w.Body.String(); body == "" {
		t.Fatal("nothing reached the client")
	}
}

// A non-streaming responder owns no part of the body — the handler
// writes JSON — so every event method must be a no-op whatever the
// flag says.
func TestANonStreamingResponderWritesNothing(t *testing.T) {
	t.Parallel()
	w := &countingWriter{ResponseRecorder: httptest.NewRecorder()}
	r := &restResponder{w: w, flusher: nopFlusher{}, streaming: false}

	_ = r.Typing(t.Context())
	_ = r.Interim(t.Context(), "still working")
	_ = r.forceEvent("final", map[string]any{})

	if w.totalWritten != 0 {
		t.Errorf("%d writes from a non-streaming responder", w.totalWritten)
	}
}

type nopFlusher struct{}

func (nopFlusher) Flush() {}
