package gateway

import (
	"context"
	"sync"
	"testing"
	"time"
)

// The bug the gate exists for: Load → run → Append was not atomic, so
// two messages arriving during one turn both read the same prior
// history and both appended. This reproduces that shape — a shared
// "transcript" read at the start of a turn and written at the end —
// and asserts the gate makes it impossible.
func TestSerialTurnsDoNotInterleave(t *testing.T) {
	t.Parallel()
	g := NewTurnGate(QueueSerial, 0, nil)

	var mu sync.Mutex
	transcript := []string{}

	const n = 5
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lease, d := g.Acquire(context.Background(), "chat:1", "turn", "msg")
			if d != Admitted {
				t.Errorf("serial dropped a message: %v", d)
				return
			}
			defer lease.Release()

			// Read prior history, "think", then append — the exact
			// window that used to interleave.
			mu.Lock()
			seen := len(transcript)
			mu.Unlock()

			time.Sleep(2 * time.Millisecond)

			mu.Lock()
			if len(transcript) != seen {
				t.Errorf("history changed under a running turn: saw %d, now %d", seen, len(transcript))
			}
			transcript = append(transcript, "turn")
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if len(transcript) != n {
		t.Errorf("transcript has %d turns, want %d", len(transcript), n)
	}
}

// Different sessions must not block each other, or one slow
// conversation stalls every other user.
func TestDifferentSessionsRunConcurrently(t *testing.T) {
	t.Parallel()
	g := NewTurnGate(QueueSerial, 0, nil)

	first, d := g.Acquire(context.Background(), "chat:1", "turn", "a")
	if d != Admitted {
		t.Fatal("first not admitted")
	}
	defer first.Release()

	done := make(chan struct{})
	go func() {
		lease, d := g.Acquire(context.Background(), "chat:2", "turn", "b")
		if d == Admitted {
			lease.Release()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a turn on chat:2 blocked behind chat:1")
	}
}

func TestOffDropsMidTurnMessages(t *testing.T) {
	t.Parallel()
	g := NewTurnGate(QueueOff, 0, nil)

	held, d := g.Acquire(context.Background(), "chat:1", "turn", "first")
	if d != Admitted {
		t.Fatal("first not admitted")
	}

	if _, d := g.Acquire(context.Background(), "chat:1", "turn", "second"); d != Dropped {
		t.Errorf("mid-turn message got %v, want Dropped", d)
	}

	// Once the turn ends the next message runs normally.
	held.Release()
	lease, d := g.Acquire(context.Background(), "chat:1", "turn", "third")
	if d != Admitted {
		t.Fatalf("post-turn message got %v, want Admitted", d)
	}
	lease.Release()
}

// Latest keeps the newest queued message and discards what it
// overtook — and must report the discard, since nothing else will.
func TestLatestSupersedesQueuedMessages(t *testing.T) {
	t.Parallel()
	g := NewTurnGate(QueueLatest, 0, nil)

	held, d := g.Acquire(context.Background(), "chat:1", "turn", "running")
	if d != Admitted {
		t.Fatal("first not admitted")
	}

	firstQueued := make(chan Disposition, 1)
	go func() {
		_, d := g.Acquire(context.Background(), "chat:1", "turn", "stale")
		firstQueued <- d
	}()

	// Let the stale one queue before the newer one overtakes it.
	waitUntil(t, func() bool { return g.queueLen("chat:1") == 1 }, "stale message never queued")

	secondQueued := make(chan *Lease, 1)
	go func() {
		lease, d := g.Acquire(context.Background(), "chat:1", "turn", "fresh")
		if d == Admitted {
			secondQueued <- lease
		}
	}()
	waitUntil(t, func() bool { return g.queueLen("chat:1") == 1 }, "fresh message never queued")

	select {
	case d := <-firstQueued:
		if d != Dropped {
			t.Errorf("overtaken message got %v, want Dropped", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("overtaken message was never told it was dropped")
	}

	held.Release()
	select {
	case lease := <-secondQueued:
		if len(lease.Batch) != 1 || lease.Batch[0] != "fresh" {
			t.Errorf("batch = %v, want [fresh]", lease.Batch)
		}
		if lease.Superseded != 1 {
			t.Errorf("Superseded = %d, want 1 — the discard must be observable", lease.Superseded)
		}
		lease.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("newest message never ran")
	}
}

// Debounce is the mode that matches how people type: three fragments
// in quick succession become one turn answering all three.
func TestDebounceFoldsRapidFragments(t *testing.T) {
	t.Parallel()
	g := NewTurnGate(QueueDebounce, 80*time.Millisecond, nil)

	type result struct {
		lease *Lease
		d     Disposition
	}
	results := make(chan result, 3)

	go func() {
		lease, d := g.Acquire(context.Background(), "chat:1", "turn", "what is")
		results <- result{lease, d}
	}()
	// The follow-ups land inside the fold window.
	time.Sleep(20 * time.Millisecond)
	go func() {
		lease, d := g.Acquire(context.Background(), "chat:1", "turn", "the plan")
		results <- result{lease, d}
	}()
	time.Sleep(10 * time.Millisecond)
	go func() {
		lease, d := g.Acquire(context.Background(), "chat:1", "turn", "for today")
		results <- result{lease, d}
	}()

	var admitted *Lease
	folded := 0
	for range 3 {
		select {
		case r := <-results:
			switch r.d {
			case Admitted:
				if admitted != nil {
					t.Fatal("two turns admitted; the fragments should share one")
				}
				admitted = r.lease
			case Folded:
				folded++
			default:
				t.Errorf("fragment got %v, want Admitted or Folded", r.d)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("debounce never resolved")
		}
	}

	if admitted == nil {
		t.Fatal("no turn ran")
	}
	defer admitted.Release()
	if folded != 2 {
		t.Errorf("folded %d fragments, want 2", folded)
	}
	if len(admitted.Batch) != 3 {
		t.Errorf("batch = %v, want all three fragments — a folded fragment that never reaches the turn is silently lost", admitted.Batch)
	}
}

// A caller that gives up while queued must leave the session usable.
// Deterministic half: the cancellation lands well before any
// hand-off, so the waiter is removed from the queue.
func TestAbandonedWaiterLeavesTheQueue(t *testing.T) {
	t.Parallel()
	g := NewTurnGate(QueueSerial, 0, nil)

	held, d := g.Acquire(context.Background(), "chat:1", "turn", "running")
	if d != Admitted {
		t.Fatal("first not admitted")
	}

	ctx, cancel := context.WithCancel(context.Background())
	queued := make(chan Disposition, 1)
	go func() {
		// A caller that gave up does not release — as far as it is
		// concerned its context died. So Admitted must never come
		// back on a context that is already dead.
		_, d := g.Acquire(ctx, "chat:1", "turn", "abandoned")
		queued <- d
	}()
	waitUntil(t, func() bool { return g.queueLen("chat:1") == 1 }, "message never queued")

	cancel()
	if d := <-queued; d != Dropped {
		t.Fatalf("abandoned waiter got %v, want Dropped", d)
	}
	waitUntil(t, func() bool { return g.queueLen("chat:1") == 0 }, "abandoned waiter stayed in the queue")

	held.Release()
	lease, d := g.Acquire(context.Background(), "chat:1", "turn", "next")
	if d != Admitted {
		t.Fatalf("session unusable after an abandoned waiter: %v", d)
	}
	lease.Release()
}

// The hard half, which cannot be made deterministic: the verdict can
// be delivered between the context firing and the gate taking its
// lock. Whichever way that lands, the session must stay usable — a
// waiter that is handed ownership and walks away wedges the
// conversation permanently.
//
// The disposition is deliberately not asserted. Both answers are
// correct depending on which side won, and pinning one would be
// testing the scheduler rather than the gate.
func TestCancelRacingHandoffNeverWedgesTheSession(t *testing.T) {
	t.Parallel()

	for i := range 200 {
		g := NewTurnGate(QueueSerial, 0, nil)
		held, d := g.Acquire(context.Background(), "chat:1", "turn", "running")
		if d != Admitted {
			t.Fatal("first not admitted")
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			lease, d := g.Acquire(ctx, "chat:1", "turn", "racing")
			// A correct caller always releases what it was given.
			if d == Admitted {
				lease.Release()
			}
			close(done)
		}()
		waitUntil(t, func() bool { return g.queueLen("chat:1") == 1 }, "message never queued")

		go cancel()
		held.Release()
		<-done

		// Whatever happened, the next turn must be able to start.
		next := make(chan Disposition, 1)
		go func() {
			lease, d := g.Acquire(context.Background(), "chat:1", "turn", "next")
			if d == Admitted {
				lease.Release()
			}
			next <- d
		}()
		select {
		case <-next:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: session wedged — ownership was taken and never released", i)
		}
		cancel()
	}
}

func TestParseQueueMode(t *testing.T) {
	t.Parallel()
	cases := map[string]QueueMode{
		"serial":   QueueSerial,
		"latest":   QueueLatest,
		"DEBOUNCE": QueueDebounce,
		"\toff\n":  QueueOff, // operators paste from YAML; trim it
		// Anything unrecognised must land on the mode that drops
		// nothing — a typo should not silently start discarding
		// people's messages.
		"":        QueueSerial,
		"garbage": QueueSerial,
		"true":    QueueSerial,
	}
	for in, want := range cases {
		if got := ParseQueueMode(in); got != want {
			t.Errorf("ParseQueueMode(%q) = %q, want %q", in, got, want)
		}
	}
}

// queueLen reports how many turns are waiting on a session. Test-only
// observability — the gate deliberately exposes no queue state to
// production callers.
func (g *TurnGate) queueLen(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if st := g.sessions[key]; st != nil {
		return len(st.waiters)
	}
	return 0
}

func waitUntil(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(msg)
}
