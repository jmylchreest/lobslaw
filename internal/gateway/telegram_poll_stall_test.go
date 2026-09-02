package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// A stalled getUpdates must cost one backoff, not the bot.
//
// getUpdates gives each request a deadline of its own, so a request
// Telegram never answers comes back as context.DeadlineExceeded while
// the loop's own context is untouched. Reading that as "we are
// shutting down" ended polling permanently: singleton.Run treats a
// clean return as finished and restarts the loop only on an ownership
// change, which a single-node cluster never has. The bot stayed up,
// answered nothing, and logged nothing about it.
func TestPollSurvivesAStalledRequest(t *testing.T) {
	t.Parallel()
	agent := newAgentFor(t, compute.MockResponse{Content: "still here"})
	batch := []byte(`[{"update_id":100,"message":{"message_id":1,"chat":{"id":42,"type":"private"},"from":{"id":7,"username":"alice"},"text":"after the stall"}}]`)
	h := newPollHarness(t, agent, [][]byte{batch})

	// A deadline short enough to fire in a test, and a stall that
	// outlasts it. The backoff comes down with them: sitting through
	// a real one-second wait made the assertion's own deadline a
	// guess about how loaded the runner is, and the guess is what
	// failed — this test flaked in CI for exactly that reason.
	h.handler.pollTimeout = 20 * time.Millisecond
	h.handler.pollSlack = 20 * time.Millisecond
	h.handler.pollBackoff = 10 * time.Millisecond
	h.mu.Lock()
	h.stallOnce = true
	h.stallFor = 500 * time.Millisecond
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.handler.RunLongPoll(ctx) }()

	waitUntil(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.sent) > 0
	}, "the poll loop never delivered the update after a stalled request — it gave up instead of backing off")

	cancel()
	<-done

	h.mu.Lock()
	sent := h.sent
	h.mu.Unlock()
	if len(sent) != 1 || sent[0].Text != "still here" {
		t.Fatalf("sent = %+v; want one reply %q", sent, "still here")
	}
}
