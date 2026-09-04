package node

import (
	"testing"
)

// The callback runs inside FSM.Apply, under the FSM's own lock. If it
// ever blocked, a full wake channel would stall raft itself.
func TestNotifyMaterialiseNeverBlocks(t *testing.T) {
	t.Parallel()
	n := &Node{materialiseWake: make(chan struct{}, 1)}

	// Far more sends than the buffer holds. Any blocking send here
	// deadlocks the test rather than failing it, which is the point:
	// this is the property that would take the cluster down.
	for range 100 {
		n.notifyMaterialise()
	}

	if got := len(n.materialiseWake); got != 1 {
		t.Errorf("wake channel holds %d requests; a burst of applies must collapse to one pass", got)
	}
}

// A queued wake is not lost: the pass that answers it reads the store
// when it runs, so it sees everything that arrived while it was queued.
func TestMaterialiseWakeIsCoalescedNotDropped(t *testing.T) {
	t.Parallel()
	n := &Node{materialiseWake: make(chan struct{}, 1)}

	n.notifyMaterialise()
	<-n.materialiseWake // a pass starts

	// More changes land while it runs.
	n.notifyMaterialise()
	select {
	case <-n.materialiseWake:
	default:
		t.Error("a change arriving during a pass was dropped; it would wait for the backstop tick")
	}
}
