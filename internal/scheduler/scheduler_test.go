package scheduler

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// singleNodeRaft brings up a single-voter in-proc Raft cluster. Much
// cheaper than the mTLS gRPC fixture; sufficient to verify the
// FSM + scheduler dance end-to-end. Takes testing.TB so both tests
// and benchmarks can call it.
func singleNodeRaft(tb testing.TB, nodeID string) (*memory.RaftNode, *memory.Store) {
	tb.Helper()
	dir := tb.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		tb.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(dir, "state.db"), key)
	if err != nil {
		tb.Fatal(err)
	}
	fsm := memory.NewFSM(store)
	localAddr := raft.ServerAddress(nodeID)
	_, inmem := raft.NewInmemTransport(localAddr)
	node, err := memory.NewRaft(memory.RaftConfig{
		NodeID:    nodeID,
		LocalAddr: localAddr,
		DataDir:   dir,
		Bootstrap: true,
		Transport: inmem,
	}, fsm)
	if err != nil {
		tb.Fatalf("NewRaft: %v", err)
	}
	if err := node.WaitForLeader(5 * time.Second); err != nil {
		_ = node.Shutdown()
		_ = store.Close()
		tb.Fatalf("WaitForLeader: %v", err)
	}
	tb.Cleanup(func() {
		_ = node.Shutdown()
		_ = store.Close()
	})
	return node, store
}

// seedTask inserts a ScheduledTaskRecord directly via a PUT — lets
// tests stage state without scheduling through the cron parser.
func seedTask(tb testing.TB, node *memory.RaftNode, task *lobslawv1.ScheduledTaskRecord) {
	tb.Helper()
	entry := &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT,
		Id: task.Id,
		Payload: &lobslawv1.LogEntry_ScheduledTask{
			ScheduledTask: task,
		},
	}
	data, err := proto.Marshal(entry)
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

// loadTask reads back the current task state from the store so tests
// can assert on claim fields / LastRun / NextRun.
func loadTask(t *testing.T, store *memory.Store, id string) *lobslawv1.ScheduledTaskRecord {
	t.Helper()
	raw, err := store.Get(memory.BucketScheduledTasks, id)
	if err != nil {
		t.Fatal(err)
	}
	var r lobslawv1.ScheduledTaskRecord
	if err := proto.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

// --- Construction ---------------------------------------------------------

func TestNewSchedulerRequiresNodeID(t *testing.T) {
	t.Parallel()
	node, _ := singleNodeRaft(t, "n1")
	_, err := NewScheduler(Config{}, node, NewHandlerRegistry())
	if err == nil {
		t.Error("missing NodeID should fail construction")
	}
}

func TestNewSchedulerRequiresRaft(t *testing.T) {
	t.Parallel()
	_, err := NewScheduler(Config{NodeID: "n"}, nil, NewHandlerRegistry())
	if err == nil {
		t.Error("nil raft should fail construction")
	}
}

// TestSchedulerWireFSMCallback — NewScheduler installs the FSM's
// scheduler-change callback, so a write via Raft.Apply wakes the
// scheduler even if we're not inside its Run loop.
func TestSchedulerWireFSMCallback(t *testing.T) {
	t.Parallel()
	node, _ := singleNodeRaft(t, "n1")
	s, err := NewScheduler(Config{NodeID: "n1"}, node, NewHandlerRegistry())
	if err != nil {
		t.Fatal(err)
	}
	seedTask(t, node, &lobslawv1.ScheduledTaskRecord{
		Id: "t1", Schedule: "* * * * *", Enabled: true,
	})
	// wakeCh is buffered(1); if the FSM fired the callback we should
	// see a queued wake.
	select {
	case <-s.wakeCh:
	case <-time.After(time.Second):
		t.Error("FSM callback didn't wake the scheduler after a PUT")
	}
}

// TestSchedulerFiresDueTask — the happy path. A past-due task gets
// claimed + the handler runs + claim clears + NextRun advances.
func TestSchedulerFiresDueTask(t *testing.T) {
	t.Parallel()
	node, store := singleNodeRaft(t, "n1")

	reg := NewHandlerRegistry()
	var fired atomic.Int32
	_ = reg.RegisterTask("echo", func(ctx context.Context, task *lobslawv1.ScheduledTaskRecord) error {
		fired.Add(1)
		return nil
	})

	s, err := NewScheduler(Config{
		NodeID:   "n1",
		ClaimTTL: 10 * time.Second,
		MaxSleep: 100 * time.Millisecond,
	}, node, reg)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	seedTask(t, node, &lobslawv1.ScheduledTaskRecord{
		Id:         "t1",
		Name:       "echo-every-minute",
		Schedule:   "* * * * *",
		HandlerRef: "echo",
		Enabled:    true,
		NextRun:    timestamppb.New(now.Add(-time.Second)),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runDone := make(chan struct{})
	go func() {
		_ = s.Run(ctx)
		close(runDone)
	}()

	// Wait for handler to fire.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fired.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fired.Load() == 0 {
		t.Fatal("handler never fired")
	}

	// Poll for completion: the post-fire write clears ClaimedBy.
	var stored *lobslawv1.ScheduledTaskRecord
	for time.Now().Before(deadline) {
		stored = loadTask(t, store, "t1")
		if stored.ClaimedBy == "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if stored == nil || stored.ClaimedBy != "" {
		t.Errorf("claim not cleared after fire: %+v", stored)
	}
	if stored.LastRun == nil || stored.LastRun.AsTime().Before(now) {
		t.Errorf("LastRun not advanced: %+v", stored.LastRun)
	}
	if stored.NextRun == nil || !stored.NextRun.AsTime().After(now) {
		t.Errorf("NextRun not advanced to future: %+v", stored.NextRun)
	}

	cancel()
	<-runDone
}

// TestSchedulerSkipsDisabledTask — a task marked Enabled=false MUST
// be skipped even when its NextRun is in the past.
func TestSchedulerSkipsDisabledTask(t *testing.T) {
	t.Parallel()
	node, _ := singleNodeRaft(t, "n1")
	reg := NewHandlerRegistry()
	var fired atomic.Int32
	_ = reg.RegisterTask("noop", func(_ context.Context, _ *lobslawv1.ScheduledTaskRecord) error {
		fired.Add(1)
		return nil
	})
	s, _ := NewScheduler(Config{NodeID: "n1", MaxSleep: 100 * time.Millisecond}, node, reg)

	seedTask(t, node, &lobslawv1.ScheduledTaskRecord{
		Id: "t1", Schedule: "* * * * *", HandlerRef: "noop",
		Enabled: false,
		NextRun: timestamppb.New(time.Now().Add(-time.Minute)),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = s.Run(ctx)

	if fired.Load() != 0 {
		t.Errorf("disabled task fired %d times", fired.Load())
	}
}

// TestSchedulerUnknownHandlerReleasesClaim — when a fired task
// references a HandlerRef that isn't registered, the claim must be
// released so a sibling node (with a matching handler) can pick up.
func TestSchedulerUnknownHandlerReleasesClaim(t *testing.T) {
	t.Parallel()
	node, store := singleNodeRaft(t, "n1")
	s, _ := NewScheduler(Config{
		NodeID:   "n1",
		MaxSleep: 100 * time.Millisecond,
	}, node, NewHandlerRegistry())

	seedTask(t, node, &lobslawv1.ScheduledTaskRecord{
		Id: "t1", Schedule: "* * * * *", HandlerRef: "missing",
		Enabled: true,
		NextRun: timestamppb.New(time.Now().Add(-time.Second)),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runDone := make(chan struct{})
	go func() { _ = s.Run(ctx); close(runDone) }()

	// Poll for the claim-release post-fire.
	deadline := time.Now().Add(1500 * time.Millisecond)
	var stored *lobslawv1.ScheduledTaskRecord
	for time.Now().Before(deadline) {
		stored = loadTask(t, store, "t1")
		// After release ClaimedBy is empty AND the release handler
		// did NOT advance LastRun (we only advance on successful fire).
		if stored.ClaimedBy == "" && stored.LastRun == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if stored.ClaimedBy != "" {
		t.Errorf("unknown handler should clear the claim; got %q", stored.ClaimedBy)
	}
	if stored.LastRun != nil {
		t.Errorf("unknown handler should NOT advance LastRun; got %v", stored.LastRun)
	}

	cancel()
	<-runDone
}

// TestSchedulerFiresDueCommitment — same happy-path as tasks but for
// one-shot commitments. Post-fire the commitment is Status=done.
func TestSchedulerFiresDueCommitment(t *testing.T) {
	t.Parallel()
	node, store := singleNodeRaft(t, "n1")
	reg := NewHandlerRegistry()
	var fired atomic.Int32
	_ = reg.RegisterCommitment("ping", func(_ context.Context, _ *lobslawv1.AgentCommitment) error {
		fired.Add(1)
		return nil
	})
	s, _ := NewScheduler(Config{
		NodeID:   "n1",
		MaxSleep: 100 * time.Millisecond,
	}, node, reg)

	// Seed a pending commitment due now.
	commitment := &lobslawv1.AgentCommitment{
		Id: "c1", Status: "pending", HandlerRef: "ping",
		DueAt: timestamppb.New(time.Now().Add(-time.Second)),
	}
	entry := &lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      "c1",
		Payload: &lobslawv1.LogEntry_Commitment{Commitment: commitment},
	}
	data, _ := proto.Marshal(entry)
	res, err := node.Apply(data, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ferr, ok := res.(error); ok && ferr != nil {
		t.Fatal(ferr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runDone := make(chan struct{})
	go func() { _ = s.Run(ctx); close(runDone) }()

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if fired.Load() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if fired.Load() == 0 {
		t.Fatal("commitment handler never fired")
	}

	// Status should be done.
	for time.Now().Before(deadline) {
		raw, err := store.Get(memory.BucketCommitments, "c1")
		if err == nil {
			var c lobslawv1.AgentCommitment
			if perr := proto.Unmarshal(raw, &c); perr == nil && c.Status == "done" {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	raw, _ := store.Get(memory.BucketCommitments, "c1")
	var final lobslawv1.AgentCommitment
	_ = proto.Unmarshal(raw, &final)
	if final.Status != "done" {
		t.Errorf("commitment status after fire: %q", final.Status)
	}

	cancel()
	<-runDone
}

// TestSchedulerNotifyWakesEarly — seed with a due-far-in-future
// task so the scheduler sleeps the full MaxSleep. Then add a
// due-now task and verify the scheduler wakes promptly and fires
// it without waiting out the original sleep.
func TestSchedulerNotifyWakesEarly(t *testing.T) {
	t.Parallel()
	node, _ := singleNodeRaft(t, "n1")
	reg := NewHandlerRegistry()
	var fired atomic.Int32
	_ = reg.RegisterTask("noop", func(_ context.Context, _ *lobslawv1.ScheduledTaskRecord) error {
		fired.Add(1)
		return nil
	})
	s, _ := NewScheduler(Config{
		NodeID:   "n1",
		MaxSleep: 5 * time.Second, // long default so we rely on wake signal
	}, node, reg)

	// Seed a far-future task first.
	seedTask(t, node, &lobslawv1.ScheduledTaskRecord{
		Id: "t-later", Schedule: "* * * * *", HandlerRef: "noop",
		Enabled: true,
		NextRun: timestamppb.New(time.Now().Add(time.Hour)),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runDone := make(chan struct{})
	go func() { _ = s.Run(ctx); close(runDone) }()

	// Let the scheduler reach its long sleep.
	time.Sleep(200 * time.Millisecond)

	// Inject a due-now task. FSM callback should wake the scheduler,
	// computeSleep returns ~0, fireDue runs, handler increments.
	seedTask(t, node, &lobslawv1.ScheduledTaskRecord{
		Id: "t-now", Schedule: "* * * * *", HandlerRef: "noop",
		Enabled: true,
		NextRun: timestamppb.New(time.Now().Add(-time.Second)),
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fired.Load() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if fired.Load() == 0 {
		t.Error("NOTIFY should have woken the scheduler; handler never fired")
	}

	cancel()
	<-runDone
}

// TestSchedulerConcurrentClaimOnlyOneWins simulates the cluster-race
// scenario inside a single Raft group. Two Scheduler instances point
// at the same Raft node and compete to claim the same task. FSM's
// serialised Apply guarantees exactly one wins.
func TestSchedulerConcurrentClaimOnlyOneWins(t *testing.T) {
	t.Parallel()
	node, _ := singleNodeRaft(t, "n1")

	var firedA atomic.Int32
	regA := NewHandlerRegistry()
	_ = regA.RegisterTask("echo", func(_ context.Context, _ *lobslawv1.ScheduledTaskRecord) error {
		firedA.Add(1)
		return nil
	})
	var firedB atomic.Int32
	regB := NewHandlerRegistry()
	_ = regB.RegisterTask("echo", func(_ context.Context, _ *lobslawv1.ScheduledTaskRecord) error {
		firedB.Add(1)
		return nil
	})

	// Two schedulers sharing the raft node — simulates two nodes in
	// a cluster for the CAS-race test without a full multi-node
	// transport setup. Both will wake on any FSM change.
	sA, _ := NewScheduler(Config{
		NodeID:   "node-a",
		MaxSleep: 50 * time.Millisecond,
		ClaimTTL: time.Minute,
	}, node, regA)
	sB, _ := NewScheduler(Config{
		NodeID:   "node-b",
		MaxSleep: 50 * time.Millisecond,
		ClaimTTL: time.Minute,
	}, node, regB)

	seedTask(t, node, &lobslawv1.ScheduledTaskRecord{
		Id: "shared", Schedule: "* * * * *", HandlerRef: "echo",
		Enabled: true,
		NextRun: timestamppb.New(time.Now().Add(-time.Second)),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	aDone := make(chan struct{})
	bDone := make(chan struct{})
	go func() { _ = sA.Run(ctx); close(aDone) }()
	go func() { _ = sB.Run(ctx); close(bDone) }()

	// Wait for either handler to fire.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if firedA.Load()+firedB.Load() >= 1 {
			// Give the other a moment to (not) also fire.
			time.Sleep(100 * time.Millisecond)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	total := firedA.Load() + firedB.Load()
	if total != 1 {
		t.Errorf("expected exactly one handler fire across both schedulers; got A=%d B=%d total=%d",
			firedA.Load(), firedB.Load(), total)
	}

	cancel()
	<-aDone
	<-bDone
}
