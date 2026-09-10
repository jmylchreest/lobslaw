package scheduler

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Commitments are completed BEFORE their handler runs, which is
// at-most-once: a reminder that fires twice is worse than one that
// occasionally goes missing.
//
// That reasoning inverts for a handler polling a job already
// submitted and already being billed. Re-polling costs one cheap
// request; losing the only poll orphans the job — it keeps running,
// the artifact is never collected, and the user never hears back.
// Idempotent() opts into the other ordering, and these tests pin the
// difference.

func TestIdempotentIsOptInOnly(t *testing.T) {
	t.Parallel()
	r := NewHandlerRegistry()
	noop := func(context.Context, *lobslawv1.AgentCommitment) error { return nil }

	if err := r.RegisterCommitment("plain", noop); err != nil {
		t.Fatal(err)
	}
	if r.IsIdempotent("plain") {
		t.Error("a handler nobody thought about was treated as idempotent; the safe default is at-most-once")
	}

	if err := r.RegisterCommitment("polling", noop, Idempotent()); err != nil {
		t.Fatal(err)
	}
	if !r.IsIdempotent("polling") {
		t.Error("Idempotent() did not take effect")
	}

	// Re-registering without the option must clear it. Registration is
	// last-write-wins, and a stale idempotent flag would silently give
	// a replacement handler delivery semantics it never asked for.
	if err := r.RegisterCommitment("polling", noop); err != nil {
		t.Fatal(err)
	}
	if r.IsIdempotent("polling") {
		t.Error("re-registering without Idempotent() left the flag set")
	}
}

// The whole point of the flag: an idempotent handler must have run
// before its commitment is closed, so a crash mid-handler leaves work
// recoverable rather than marked done and lost.
func TestIdempotentHandlerRunsBeforeCompletion(t *testing.T) {
	t.Parallel()
	node, _ := singleNodeRaft(t, "n1")

	var sawStatusAtRun atomic.Value
	reg := NewHandlerRegistry()
	_ = reg.RegisterCommitment("poll", func(_ context.Context, c *lobslawv1.AgentCommitment) error {
		// Read the record as it stands in the store while we run.
		got := loadCommitment(t, node, c.Id)
		sawStatusAtRun.Store(got.Status)
		return nil
	}, Idempotent())

	s, _ := NewScheduler(Config{NodeID: "node-a", MaxSleep: 20 * time.Millisecond, ClaimTTL: time.Minute}, node, reg)
	seedCommitment(t, node, &lobslawv1.AgentCommitment{
		Id: "c1", HandlerRef: "poll", Status: "pending",
		DueAt: timestamppb.New(time.Now().Add(-time.Second)),
	})

	// Waits for the completion this test is about, rather than
	// sleeping and hoping: the handler running and its raft apply
	// landing are two events, and only the second one is what the
	// assertion below reads.
	runSchedulerUntil(t, s, func() bool {
		return loadCommitmentIfPresent(node, "c1").GetStatus() == string(statusDone)
	})

	got, _ := sawStatusAtRun.Load().(string)
	if got == string(statusDone) {
		t.Error("the commitment was already Done when the handler ran; " +
			"a crash inside the handler would lose the job")
	}
	if final := loadCommitment(t, node, "c1"); final.Status != string(statusDone) {
		t.Errorf("commitment not completed after a successful handler: status=%q", final.Status)
	}
}

// A polling handler is not finished when it returns — it is finished
// when the JOB is. RetryAfter must leave the commitment pending and
// move it, rather than closing it.
func TestRetryAfterRearmsInsteadOfCompleting(t *testing.T) {
	t.Parallel()
	node, _ := singleNodeRaft(t, "n1")

	var calls atomic.Int32
	retryAt := time.Now().Add(42 * time.Minute)
	reg := NewHandlerRegistry()
	_ = reg.RegisterCommitment("poll", func(context.Context, *lobslawv1.AgentCommitment) error {
		calls.Add(1)
		return &RetryAfter{At: retryAt, Reason: "job still running"}
	}, Idempotent())

	s, _ := NewScheduler(Config{NodeID: "node-a", MaxSleep: 20 * time.Millisecond, ClaimTTL: time.Minute}, node, reg)
	seedCommitment(t, node, &lobslawv1.AgentCommitment{
		Id: "c1", HandlerRef: "poll", Status: "pending",
		DueAt: timestamppb.New(time.Now().Add(-time.Second)),
	})

	// Waits for the RE-ARM to land, not merely for the handler to have
	// been called: the apply is a separate raft round-trip, and reading
	// between the two shows the original DueAt and an unreleased claim.
	runSchedulerUntil(t, s, func() bool {
		c := loadCommitment(t, node, "c1")
		return c != nil && c.ClaimedBy == "" && c.DueAt != nil &&
			c.DueAt.AsTime().After(time.Now().Add(30*time.Minute))
	})

	if calls.Load() == 0 {
		t.Fatal("handler never ran")
	}
	got := loadCommitment(t, node, "c1")
	if got.Status == string(statusDone) {
		t.Fatal("a retry request closed the commitment; the job is now orphaned")
	}
	if got.ClaimedBy != "" {
		t.Errorf("claim not released on re-arm (claimed_by=%q); another node cannot take the next poll", got.ClaimedBy)
	}
	if got.DueAt == nil || got.DueAt.AsTime().Before(time.Now().Add(30*time.Minute)) {
		t.Errorf("DueAt = %v, want it moved to roughly %v", got.DueAt.AsTime(), retryAt)
	}
}

// A handler that returns a real error is finished, unsuccessfully.
// Leaving it pending would retry a job the handler already gave up on.
func TestIdempotentHandlerErrorStillCompletes(t *testing.T) {
	t.Parallel()
	node, _ := singleNodeRaft(t, "n1")

	reg := NewHandlerRegistry()
	_ = reg.RegisterCommitment("poll", func(context.Context, *lobslawv1.AgentCommitment) error {
		return errors.New("the job failed permanently")
	}, Idempotent())

	s, _ := NewScheduler(Config{NodeID: "node-a", MaxSleep: 20 * time.Millisecond, ClaimTTL: time.Minute}, node, reg)
	seedCommitment(t, node, &lobslawv1.AgentCommitment{
		Id: "c1", HandlerRef: "poll", Status: "pending",
		DueAt: timestamppb.New(time.Now().Add(-time.Second)),
	})

	runSchedulerUntil(t, s, func() bool {
		return loadCommitmentIfPresent(node, "c1").GetStatus() == string(statusDone)
	})

	if got := loadCommitment(t, node, "c1"); got.Status != string(statusDone) {
		t.Errorf("status = %q, want done — a handler that errored is finished, not retrying", got.Status)
	}
}

func TestAsRetryAfterUnwraps(t *testing.T) {
	t.Parallel()
	if _, ok := AsRetryAfter(errors.New("plain")); ok {
		t.Error("a plain error was read as a retry request")
	}
	r := RetryAfterIn(time.Minute, "because")
	if got, ok := AsRetryAfter(r); !ok || got.Reason != "because" {
		t.Errorf("AsRetryAfter(%v) = %v, %v", r, got, ok)
	}
	// Handlers wrap errors on the way out; the signal must survive it.
	wrapped := errWrap{r}
	if _, ok := AsRetryAfter(wrapped); !ok {
		t.Error("a wrapped RetryAfter was not recognised")
	}
}

type errWrap struct{ err error }

func (e errWrap) Error() string { return "wrapped: " + e.err.Error() }
func (e errWrap) Unwrap() error { return e.err }

// A re-arming handler carries state forward on the record it was
// handed, and these two tests are why that is safe to rely on.
//
// The alternative — a handler writing the record itself and then
// asking for a retry — cannot work: the write moves the revision, and
// the re-arm applies under a CAS on the revision the scheduler read,
// so the handler would lose its own re-arm to a conflict every time.
// Mutating in place is one raft write instead of two and cannot race
// with itself, and internal/node's watch handler depends on it.
func TestRearmCarriesHandlerMutations(t *testing.T) {
	t.Parallel()
	node, _ := singleNodeRaft(t, "n1")

	reg := NewHandlerRegistry()
	_ = reg.RegisterCommitment("stateful", func(_ context.Context, c *lobslawv1.AgentCommitment) error {
		if c.Params == nil {
			c.Params = map[string]string{}
		}
		c.Params["observed"] = "price: 212.00 GBP"
		return RetryAfterIn(42*time.Minute, "still watching")
	}, Idempotent())

	s, _ := NewScheduler(Config{NodeID: "node-a", MaxSleep: 20 * time.Millisecond, ClaimTTL: time.Minute}, node, reg)
	seedCommitment(t, node, &lobslawv1.AgentCommitment{
		Id: "c1", HandlerRef: "stateful", Status: "pending",
		DueAt: timestamppb.New(time.Now().Add(-time.Second)),
	})

	runSchedulerUntil(t, s, func() bool {
		c := loadCommitment(t, node, "c1")
		return c != nil && c.ClaimedBy == "" && c.Params["observed"] != ""
	})

	got := loadCommitment(t, node, "c1")
	if got.Params["observed"] != "price: 212.00 GBP" {
		t.Fatalf("the re-arm dropped what the handler recorded (params=%v); "+
			"a handler that carries state between fires has nowhere else to put it", got.Params)
	}
	if got.Status == string(statusDone) {
		t.Error("a retry request closed the commitment")
	}
}

// The same guarantee on the terminal path: a handler that finishes
// after recording something must not lose the recording.
func TestCompletionCarriesHandlerMutations(t *testing.T) {
	t.Parallel()
	node, _ := singleNodeRaft(t, "n1")

	reg := NewHandlerRegistry()
	_ = reg.RegisterCommitment("stateful", func(_ context.Context, c *lobslawv1.AgentCommitment) error {
		if c.Params == nil {
			c.Params = map[string]string{}
		}
		c.Params["suspended_because"] = "the check never reported"
		return nil
	}, Idempotent())

	s, _ := NewScheduler(Config{NodeID: "node-a", MaxSleep: 20 * time.Millisecond, ClaimTTL: time.Minute}, node, reg)
	seedCommitment(t, node, &lobslawv1.AgentCommitment{
		Id: "c1", HandlerRef: "stateful", Status: "pending",
		DueAt: timestamppb.New(time.Now().Add(-time.Second)),
	})

	runSchedulerUntil(t, s, func() bool {
		c := loadCommitment(t, node, "c1")
		return c != nil && c.Status == string(statusDone)
	})

	got := loadCommitment(t, node, "c1")
	if got.Params["suspended_because"] != "the check never reported" {
		t.Errorf("completion dropped what the handler recorded (params=%v); "+
			"a watch that stops has to be able to say why", got.Params)
	}
}
