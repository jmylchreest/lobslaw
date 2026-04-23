package gateway

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Most tests drive the registry with long TTLs so auto-timeout
// doesn't race with the assertions; timeout-specific tests use
// short TTLs explicitly.
const longTTL = 5 * time.Minute

func TestPromptRegistryCreateReturnsIDAndSnapshot(t *testing.T) {
	t.Parallel()
	r := NewPromptRegistry()

	p, err := r.Create("turn-1", "dangerous thing", "rest", longTTL)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID == "" || len(p.ID) != 32 {
		t.Errorf("ID should be 32 hex chars; got %q (len=%d)", p.ID, len(p.ID))
	}
	if p.TurnID != "turn-1" || p.Reason != "dangerous thing" || p.Channel != "rest" {
		t.Errorf("field round-trip: %+v", p)
	}
	if p.Decision != PromptPending {
		t.Errorf("new prompt should be Pending; got %s", p.Decision)
	}
	if !p.ExpiresAt.After(p.CreatedAt) {
		t.Error("ExpiresAt must be after CreatedAt")
	}
}

func TestPromptRegistryCreateIDsAreUnique(t *testing.T) {
	t.Parallel()
	r := NewPromptRegistry()
	seen := make(map[string]struct{}, 200)
	for range 200 {
		p, err := r.Create("t", "r", "rest", longTTL)
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := seen[p.ID]; dup {
			t.Fatalf("duplicate ID: %s", p.ID)
		}
		seen[p.ID] = struct{}{}
	}
}

func TestPromptRegistryGetUnknown(t *testing.T) {
	t.Parallel()
	r := NewPromptRegistry()
	_, err := r.Get("nonexistent")
	if !errors.Is(err, ErrPromptNotFound) {
		t.Errorf("unknown id should return ErrPromptNotFound; got %v", err)
	}
}

func TestPromptRegistryGetReturnsSnapshot(t *testing.T) {
	t.Parallel()
	r := NewPromptRegistry()
	p, _ := r.Create("t", "r", "rest", longTTL)

	snap, err := r.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Snapshot pointer must differ from the registry's internal
	// pointer so external mutation can't taint state.
	if snap == p {
		t.Error("Get must return a snapshot, not the registered pointer")
	}
	if snap.ID != p.ID || snap.Decision != p.Decision {
		t.Errorf("snapshot mismatch: %+v vs %+v", snap, p)
	}
}

func TestPromptRegistryResolveApproved(t *testing.T) {
	t.Parallel()
	r := NewPromptRegistry()
	p, _ := r.Create("t", "r", "rest", longTTL)

	if err := r.Resolve(p.ID, PromptApproved); err != nil {
		t.Fatal(err)
	}
	snap, _ := r.Get(p.ID)
	if snap.Decision != PromptApproved {
		t.Errorf("Decision should be Approved; got %s", snap.Decision)
	}
}

func TestPromptRegistryResolveDenied(t *testing.T) {
	t.Parallel()
	r := NewPromptRegistry()
	p, _ := r.Create("t", "r", "rest", longTTL)

	if err := r.Resolve(p.ID, PromptDenied); err != nil {
		t.Fatal(err)
	}
	snap, _ := r.Get(p.ID)
	if snap.Decision != PromptDenied {
		t.Errorf("Decision should be Denied; got %s", snap.Decision)
	}
}

// TestPromptRegistryResolveIdempotentFirstWriterWins — second Resolve
// call after a decision is already recorded must 409, not silently
// overwrite. Prevents "user approves, then denies" race replay.
func TestPromptRegistryResolveIdempotentFirstWriterWins(t *testing.T) {
	t.Parallel()
	r := NewPromptRegistry()
	p, _ := r.Create("t", "r", "rest", longTTL)

	if err := r.Resolve(p.ID, PromptApproved); err != nil {
		t.Fatal(err)
	}
	err := r.Resolve(p.ID, PromptDenied)
	if !errors.Is(err, ErrPromptResolved) {
		t.Errorf("second Resolve should return ErrPromptResolved; got %v", err)
	}
	snap, _ := r.Get(p.ID)
	if snap.Decision != PromptApproved {
		t.Errorf("first decision must win; got %s", snap.Decision)
	}
}

func TestPromptRegistryResolveUnknown(t *testing.T) {
	t.Parallel()
	r := NewPromptRegistry()
	err := r.Resolve("nonexistent", PromptApproved)
	if !errors.Is(err, ErrPromptNotFound) {
		t.Errorf("unknown id should return ErrPromptNotFound; got %v", err)
	}
}

// TestPromptRegistryResolveRejectsInvalidDecision — only Approved or
// Denied are user-facing; Pending and TimedOut are internal.
func TestPromptRegistryResolveRejectsInvalidDecision(t *testing.T) {
	t.Parallel()
	r := NewPromptRegistry()
	p, _ := r.Create("t", "r", "rest", longTTL)

	for _, bad := range []PromptDecision{PromptPending, PromptTimedOut} {
		if err := r.Resolve(p.ID, bad); err == nil {
			t.Errorf("Resolve must reject %s", bad)
		}
	}
}

func TestPromptRegistryWaitReturnsOnResolve(t *testing.T) {
	t.Parallel()
	r := NewPromptRegistry()
	p, _ := r.Create("t", "r", "rest", longTTL)

	var wg sync.WaitGroup
	var gotDecision PromptDecision
	var gotErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		gotDecision, gotErr = r.Wait(context.Background(), p.ID)
	}()

	// Give Wait() a moment to reach the select, then resolve.
	time.Sleep(10 * time.Millisecond)
	if err := r.Resolve(p.ID, PromptApproved); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	if gotErr != nil {
		t.Errorf("unexpected err: %v", gotErr)
	}
	if gotDecision != PromptApproved {
		t.Errorf("Wait should return Approved; got %s", gotDecision)
	}
}

// TestPromptRegistryWaitReturnsImmediatelyWhenAlreadyResolved — Wait
// mustn't block on a prompt that resolved before the caller arrives.
func TestPromptRegistryWaitReturnsImmediatelyWhenAlreadyResolved(t *testing.T) {
	t.Parallel()
	r := NewPromptRegistry()
	p, _ := r.Create("t", "r", "rest", longTTL)
	_ = r.Resolve(p.ID, PromptDenied)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	d, err := r.Wait(ctx, p.ID)
	if err != nil {
		t.Fatalf("Wait on already-resolved should not err; got %v", err)
	}
	if d != PromptDenied {
		t.Errorf("want Denied; got %s", d)
	}
}

func TestPromptRegistryWaitUnknown(t *testing.T) {
	t.Parallel()
	r := NewPromptRegistry()
	_, err := r.Wait(context.Background(), "nonexistent")
	if !errors.Is(err, ErrPromptNotFound) {
		t.Errorf("Wait on unknown should return ErrPromptNotFound; got %v", err)
	}
}

// TestPromptRegistryWaitContextCancelReturnsPending — a cancelled
// ctx returns the context error. Distinguishes "I stopped waiting"
// from "resolved".
func TestPromptRegistryWaitContextCancelReturnsPending(t *testing.T) {
	t.Parallel()
	r := NewPromptRegistry()
	p, _ := r.Create("t", "r", "rest", longTTL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	d, err := r.Wait(ctx, p.ID)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx should return ctx.Err(); got %v", err)
	}
	if d != PromptPending {
		t.Errorf("cancelled Wait should report Pending; got %s", d)
	}
}

// TestPromptRegistryAutoTimeoutResolves — the TTL-fire goroutine
// transitions Pending → TimedOut without any user interaction.
func TestPromptRegistryAutoTimeoutResolves(t *testing.T) {
	t.Parallel()
	r := NewPromptRegistry()
	p, _ := r.Create("t", "r", "rest", 50*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	d, err := r.Wait(ctx, p.ID)
	if err != nil {
		t.Fatalf("Wait should unblock on timeout; got %v", err)
	}
	if d != PromptTimedOut {
		t.Errorf("auto-timeout should produce TimedOut; got %s", d)
	}
}

// TestPromptRegistryTimeoutDoesNotOverwriteUserResolution — race
// between user resolve and timer fire: first writer wins, so the
// user's Approve stands even if the timer tick is close.
func TestPromptRegistryTimeoutDoesNotOverwriteUserResolution(t *testing.T) {
	t.Parallel()
	r := NewPromptRegistry()
	p, _ := r.Create("t", "r", "rest", 20*time.Millisecond)

	// Resolve BEFORE the timer fires.
	if err := r.Resolve(p.ID, PromptApproved); err != nil {
		t.Fatal(err)
	}
	// Give the timer time to tick (and be ignored).
	time.Sleep(50 * time.Millisecond)

	snap, _ := r.Get(p.ID)
	if snap.Decision != PromptApproved {
		t.Errorf("user Approve must survive timeout; got %s", snap.Decision)
	}
}

func TestPromptRegistryReapRemovesResolvedAgedOut(t *testing.T) {
	t.Parallel()
	r := NewPromptRegistry()
	// Short TTL so ExpiresAt is in the past by the time Reap runs.
	p1, _ := r.Create("t1", "r", "rest", 10*time.Millisecond)
	p2, _ := r.Create("t2", "r", "rest", longTTL)

	// Wait for p1 to auto-timeout.
	time.Sleep(40 * time.Millisecond)

	removed := r.Reap()
	if removed != 1 {
		t.Errorf("Reap should remove exactly the timed-out entry; got %d", removed)
	}
	if _, err := r.Get(p1.ID); !errors.Is(err, ErrPromptNotFound) {
		t.Error("p1 should be gone after Reap")
	}
	if _, err := r.Get(p2.ID); err != nil {
		t.Errorf("p2 (Pending, not expired) must survive Reap; got %v", err)
	}
}

// TestPromptRegistryReapSkipsPending — even if a Pending prompt's
// ExpiresAt is in the past (would be unusual; the timer normally
// resolves first), Reap does NOT remove it — the timer's job is
// to transition it, not Reap's.
func TestPromptRegistryReapSkipsPending(t *testing.T) {
	t.Parallel()
	r := NewPromptRegistry()
	p, _ := r.Create("t", "r", "rest", longTTL)

	removed := r.Reap()
	if removed != 0 {
		t.Errorf("Reap shouldn't touch Pending prompts; removed %d", removed)
	}
	if _, err := r.Get(p.ID); err != nil {
		t.Errorf("Pending prompt must survive Reap; got %v", err)
	}
}

func TestPromptDecisionStrings(t *testing.T) {
	t.Parallel()
	cases := map[PromptDecision]string{
		PromptPending:  "pending",
		PromptApproved: "approved",
		PromptDenied:   "denied",
		PromptTimedOut: "timed_out",
	}
	for d, want := range cases {
		if got := d.String(); got != want {
			t.Errorf("%d: got %q want %q", d, got, want)
		}
	}
	// Unknown value rendering.
	if got := PromptDecision(99).String(); got != "unknown" {
		t.Errorf("unknown decision: %q", got)
	}
}

// TestPromptRegistryConcurrentResolveOnlyOneWinner — fuzzes the race
// between N goroutines each calling Resolve on the same prompt.
// Exactly one wins (nil err); the rest see ErrPromptResolved.
func TestPromptRegistryConcurrentResolveOnlyOneWinner(t *testing.T) {
	t.Parallel()
	r := NewPromptRegistry()
	p, _ := r.Create("t", "r", "rest", longTTL)

	const goroutines = 32
	var wins atomic.Int32
	var losses atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range goroutines {
		d := PromptApproved
		if i%2 == 0 {
			d = PromptDenied
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := r.Resolve(p.ID, d)
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrPromptResolved):
				losses.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if wins.Load() != 1 {
		t.Errorf("exactly one goroutine should win; got %d wins / %d losses",
			wins.Load(), losses.Load())
	}
	if wins.Load()+losses.Load() != goroutines {
		t.Errorf("all goroutines should account for: %d wins + %d losses != %d",
			wins.Load(), losses.Load(), goroutines)
	}
}
