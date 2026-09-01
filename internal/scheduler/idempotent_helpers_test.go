package scheduler

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func seedCommitment(tb testing.TB, node *memory.RaftNode, c *lobslawv1.AgentCommitment) {
	tb.Helper()
	data, err := proto.Marshal(&lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      c.Id,
		Payload: &lobslawv1.LogEntry_Commitment{Commitment: c},
	})
	if err != nil {
		tb.Fatal(err)
	}
	res, err := node.Apply(data, 5*time.Second)
	if err != nil {
		tb.Fatal(err)
	}
	if ferr, ok := res.(error); ok && ferr != nil {
		tb.Fatal(ferr)
	}
}

func loadCommitment(tb testing.TB, node *memory.RaftNode, id string) *lobslawv1.AgentCommitment {
	tb.Helper()
	raw, err := node.FSM().Store().Get(memory.BucketCommitments, id)
	if err != nil {
		tb.Fatalf("load commitment %q: %v", id, err)
	}
	var c lobslawv1.AgentCommitment
	if err := proto.Unmarshal(raw, &c); err != nil {
		tb.Fatalf("unmarshal commitment %q: %v", id, err)
	}
	return &c
}

// runSchedulerUntil runs the scheduler until `settled` reports true,
// then stops it.
//
// The fixed-sleep version below races. It slept 400ms, cancelled, and
// let the caller assert — which assumes the handler runs AND its raft
// apply lands inside that window. On a loaded CI runner it did not:
// the handler had run (call count > 0) but the re-arm had not applied,
// so the assertion read the commitment's ORIGINAL DueAt and unreleased
// claim and reported the feature broken. The failure named a real
// symptom of a bug that was not there, which is the expensive kind.
func runSchedulerUntil(tb testing.TB, s *Scheduler, settled func() bool) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for !settled() {
		if time.Now().After(deadline) {
			tb.Error("scheduler did not reach the expected state in 5s")
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		tb.Fatal("scheduler did not stop")
	}
}

// loadCommitmentIfPresent reads a commitment without failing the test
// when it is not there yet.
//
// loadCommitment calls Fatal, which is right for an assertion and
// wrong inside a polling predicate: the record legitimately does not
// exist for the first few milliseconds, and failing on that would
// turn the wait into the flake it replaced.
func loadCommitmentIfPresent(node *memory.RaftNode, id string) *lobslawv1.AgentCommitment {
	raw, err := node.FSM().Store().Get(memory.BucketCommitments, id)
	if err != nil {
		return nil
	}
	var c lobslawv1.AgentCommitment
	if err := proto.Unmarshal(raw, &c); err != nil {
		return nil
	}
	return &c
}
