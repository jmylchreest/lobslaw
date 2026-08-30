package gateway

import (
	"context"
	"errors"
	"testing"
	"time"
)

// refusingLeaser stands in for a peer that already owns the
// conversation — the ordinary multi-node case, not a fault.
type refusingLeaser struct{}

func (refusingLeaser) AcquireLease(context.Context, string, string) (LeaseHandle, error) {
	return nil, errors.New("lease held by another node: " + ErrLeaseUnavailable.Error())
}

// The panic. Acquire reported Admitted with a nil lease whenever the
// debounce window ran and the cluster leaser refused, and every
// caller then did strings.Join(lease.Batch, "\n").
//
// Release is nil-safe, so the deferred cleanup survived and the
// dereference was what fell over — the crash landed in the handler,
// several frames from the gate that lied to it.
func TestFoldWindowReportsDroppedRatherThanNilAdmitted(t *testing.T) {
	t.Parallel()

	gate := NewTurnGate(QueueDebounce, 10*time.Millisecond, discardLogger()).
		WithLeaser(refusingLeaser{}, 0)

	lease, disposition := gate.Acquire(context.Background(), "slack:C1", "t1", "hello")

	if disposition == Admitted && lease == nil {
		t.Fatal("Acquire reported Admitted with a nil lease; every caller dereferences lease.Batch")
	}
	if disposition != Dropped {
		t.Errorf("disposition = %v, want Dropped when a peer holds the conversation", disposition)
	}
	// And the caller's own cleanup must stay safe either way.
	lease.Release()
}

// The same path with no leaser at all: a single node must still be
// admitted, or the fix has traded a panic for a mute bot.
func TestFoldWindowStillAdmitsWithoutALeaser(t *testing.T) {
	t.Parallel()

	gate := NewTurnGate(QueueDebounce, 10*time.Millisecond, discardLogger())
	lease, disposition := gate.Acquire(context.Background(), "slack:C1", "t1", "hello")
	if disposition != Admitted {
		t.Fatalf("disposition = %v, want Admitted on a node with no leaser", disposition)
	}
	if lease == nil {
		t.Fatal("Admitted with a nil lease")
	}
	// The batch is what the turn answers; an empty one means the
	// message the gate was handed went missing in the window.
	if len(lease.Batch) != 1 || lease.Batch[0] != "hello" {
		t.Errorf("Batch = %v, want [hello]", lease.Batch)
	}
	lease.Release()
}
