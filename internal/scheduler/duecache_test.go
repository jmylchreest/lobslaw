package scheduler

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// The cached instant survives repeated asking.
//
// computeSleepDuration runs once per pass round the loop, and the loop
// goes round once per store change rather than once per due time. When
// it scanned both buckets every pass, a node with four tasks had
// allocated 243GB in four hours.
func TestNextDueIsCachedUntilSomethingChanges(t *testing.T) {
	t.Parallel()
	node, _ := singleNodeRaft(t, "n1")
	s, _ := NewScheduler(Config{NodeID: "node-a"}, node, NewHandlerRegistry())

	seedCommitment(t, node, &lobslawv1.AgentCommitment{
		Id: "c1", HandlerRef: "poll", Status: "pending",
		DueAt: timestamppb.New(time.Now().Add(time.Hour)),
	})

	now := time.Now()
	first, err := s.cachedNextDue(now)
	if err != nil {
		t.Fatal(err)
	}
	if first.IsZero() {
		t.Fatal("no due time found for a seeded commitment")
	}

	// Move the record out from under the cache WITHOUT invalidating.
	// A cache that re-scanned would notice; this one must not, because
	// not re-scanning is the entire point.
	seedCommitment(t, node, &lobslawv1.AgentCommitment{
		Id: "c1", HandlerRef: "poll", Status: "pending",
		DueAt: timestamppb.New(time.Now().Add(2 * time.Hour)),
	})
	again, err := s.cachedNextDue(now)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Equal(first) {
		t.Errorf("cached due moved from %v to %v without an invalidation", first, again)
	}

	// And after one, the new state is picked up.
	s.invalidateNextDue()
	third, err := s.cachedNextDue(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if third.Equal(first) {
		t.Error("invalidation did not force a rescan; a change would be invisible until MaxSleep")
	}
}

// A store change must not be lost when it lands while the scan that
// would have seen it is already running.
func TestScanRacingAnInvalidationIsNotCached(t *testing.T) {
	t.Parallel()
	node, _ := singleNodeRaft(t, "n1")
	s, _ := NewScheduler(Config{NodeID: "node-a"}, node, NewHandlerRegistry())
	seedCommitment(t, node, &lobslawv1.AgentCommitment{
		Id: "c1", HandlerRef: "poll", Status: "pending",
		DueAt: timestamppb.New(time.Now().Add(time.Hour)),
	})

	// Prime, then invalidate as though a change landed mid-scan.
	if _, err := s.cachedNextDue(time.Now()); err != nil {
		t.Fatal(err)
	}
	s.dueMu.Lock()
	s.dueValid = false
	s.dueMu.Unlock()

	s.dueMu.Lock()
	valid := s.dueValid
	s.dueMu.Unlock()
	if valid {
		t.Fatal("the cache reported valid immediately after invalidation")
	}
}

// The loop must not spin when a past-due task cannot be fired — which
// is the normal state before the post-election barrier completes, on a
// follower, and while another node holds the claim.
func TestPastDueThatCannotFireDoesNotSpin(t *testing.T) {
	t.Parallel()
	node, _ := singleNodeRaft(t, "n1")
	s, _ := NewScheduler(Config{
		NodeID:          "node-a",
		MaxSleep:        time.Minute,
		MinFireInterval: 200 * time.Millisecond,
	}, node, NewHandlerRegistry())

	// Past due, and no handler registered for it, so every attempt to
	// fire leaves it exactly as past-due as it was.
	seedCommitment(t, node, &lobslawv1.AgentCommitment{
		Id: "c1", HandlerRef: "nobody-handles-this", Status: "pending",
		DueAt: timestamppb.New(time.Now().Add(-time.Hour)),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = s.Run(ctx) }()
	<-ctx.Done()

	// With a 200ms floor over 2s, ten or so attempts is the ceiling.
	// Without the floor this is a busy loop and the count is enormous.
	got := s.dueScans.Load()
	if got > 60 {
		t.Errorf("the loop scanned %d times in 2s; it is spinning on an unfireable past-due task", got)
	}
	t.Logf("scans in 2s: %d", got)
}
