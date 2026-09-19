package node

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/scheduler"
	"github.com/jmylchreest/lobslaw/internal/workforce"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestWorkforceRoutineRunsThroughScheduler(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	key, e := crypto.GenerateKey()
	if e != nil {
		t.Fatal(e)
	}
	store, e := memory.OpenStore(filepath.Join(dir, "state.db"), key)
	if e != nil {
		t.Fatal(e)
	}
	_, transport := raft.NewInmemTransport("workforce-schedule")
	node, e := memory.NewRaft(memory.RaftConfig{NodeID: "workforce-schedule", LocalAddr: "workforce-schedule", DataDir: dir, Bootstrap: true, Transport: transport}, memory.NewFSM(store))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = node.Shutdown(); _ = store.Close() })
	if e = node.WaitForLeader(5 * time.Second); e != nil {
		t.Fatal(e)
	}
	sched, e := scheduler.NewScheduler(scheduler.Config{NodeID: "workforce-schedule"}, node, scheduler.NewHandlerRegistry())
	if e != nil {
		t.Fatal(e)
	}
	n := &Node{raft: node, store: store, scheduler: sched, botSvc: memory.NewBotService(node, store)}
	if _, e = n.botSvc.Put(ctx, &lobslawv1.BotRecord{Id: "worker", Owner: "user:alice", Enabled: true}, 0); e != nil {
		t.Fatal(e)
	}
	if e = n.wireWorkforce(); e != nil {
		t.Fatal(e)
	}
	p, e := n.workforce.CreateProject(ctx, "user:alice", workforce.Project{Name: "schedule", BotIDs: []string{"worker"}, CoordinatorBotID: "worker"})
	if e != nil {
		t.Fatal(e)
	}
	r, e := n.workforce.CreateRoutine(ctx, p.Owner, p.ID, workforce.Routine{Name: "daily", Instructions: "work", Schedule: "* * * * *"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = n.workforce.ActRoutine(ctx, p.Owner, r.ID, r.Revision, "approve", workforce.Routine{}, &types.Claims{UserID: "alice"}); e != nil {
		t.Fatal(e)
	}
	raw, e := store.Get(memory.BucketScheduledTasks, "workforce:"+r.ID)
	if e != nil {
		t.Fatal(e)
	}
	var scheduled lobslawv1.ScheduledTaskRecord
	if e = proto.Unmarshal(raw, &scheduled); e != nil {
		t.Fatal(e)
	}
	scheduled.NextRun = timestamppb.New(time.Now().Add(-time.Minute))
	entry, e := proto.Marshal(&lobslawv1.LogEntry{Op: lobslawv1.LogOp_LOG_OP_CLAIM, Id: scheduled.Id, ExpectedRevision: &scheduled.Revision, Payload: &lobslawv1.LogEntry_ScheduledTask{ScheduledTask: &scheduled}})
	if e != nil {
		t.Fatal(e)
	}
	result, e := node.ApplyOrForward(ctx, entry, workforceApplyTimeout)
	if e != nil {
		t.Fatal(e)
	}
	if applyErr, ok := result.(error); ok {
		t.Fatal(applyErr)
	}
	exited := make(chan error, 1)
	go func() { exited <- sched.Run(ctx) }()
	defer func() {
		cancel()
		if e := <-exited; e != nil {
			t.Error(e)
		}
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		tasks, e := n.workforce.ListTasks(ctx, p.Owner, p.ID)
		if e != nil {
			t.Fatal(e)
		}
		if len(tasks) == 1 {
			if tasks[0].Status != workforce.StatusReady {
				t.Fatal(tasks[0])
			}
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("scheduler did not create durable workforce task")
		case <-ticker.C:
		}
	}
	if e = n.runWorkforceSchedule(ctx, &scheduled); e != nil {
		t.Fatal(e)
	}
	tasks, e := n.workforce.ListTasks(ctx, p.Owner, p.ID)
	if e != nil || len(tasks) != 1 {
		t.Fatal("replayed occurrence duplicated work", tasks, e)
	}
}
