package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// singleNodeRaft mirrors the pattern used in scheduler tests.
func singleNodeRaft(t *testing.T, nodeID string) (*memory.RaftNode, *memory.Store, *memory.FSM) {
	t.Helper()
	dir := t.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(dir, "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	fsm := memory.NewFSM(store)
	localAddr := raft.ServerAddress(nodeID)
	_, inmem := raft.NewInmemTransport(localAddr)
	node, err := memory.NewRaft(memory.RaftConfig{
		NodeID: nodeID, LocalAddr: localAddr, DataDir: dir, Bootstrap: true, Transport: inmem,
	}, fsm)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.WaitForLeader(5 * time.Second); err != nil {
		_ = node.Shutdown()
		_ = store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = node.Shutdown()
		_ = store.Close()
	})
	return node, store, fsm
}

// fakeFactory always produces a fakeMount so tests don't need a
// real filesystem for the service-level flow.
func fakeFactory(cfg *lobslawv1.StorageMount) (Mount, error) {
	return &fakeMount{
		label:   cfg.Label,
		backend: cfg.Type,
		path:    cfg.Path,
	}, nil
}

func newServiceHarness(t *testing.T) (*Service, *memory.RaftNode) {
	t.Helper()
	node, store, fsm := singleNodeRaft(t, "svc-node")
	mgr := NewManager()
	svc, err := NewService(ServiceConfig{
		Raft:      node,
		Store:     store,
		FSM:       fsm,
		Manager:   mgr,
		Factories: map[string]BackendFactory{"fake": fakeFactory},
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, node
}

func TestAddMountReplicatesThroughRaft(t *testing.T) {
	t.Parallel()
	svc, _ := newServiceHarness(t)

	_, err := svc.AddMount(context.Background(), &lobslawv1.AddMountRequest{
		Mount: &lobslawv1.StorageMount{
			Label: "shared", Type: "fake", Path: "/srv/shared",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// ListMounts reads from the replicated bucket and should see it.
	resp, err := svc.ListMounts(context.Background(), &lobslawv1.ListMountsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Mounts) != 1 || resp.Mounts[0].Label != "shared" {
		t.Errorf("ListMounts: %+v", resp.Mounts)
	}
}

func TestAddMountRejectsEmpty(t *testing.T) {
	t.Parallel()
	svc, _ := newServiceHarness(t)
	cases := []*lobslawv1.AddMountRequest{
		nil,
		{},
		{Mount: &lobslawv1.StorageMount{}},
		{Mount: &lobslawv1.StorageMount{Label: "x"}},
	}
	for i, c := range cases {
		_, err := svc.AddMount(context.Background(), c)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("case %d: want InvalidArgument; got %v", i, err)
		}
	}
}

func TestAddMountRejectsUnknownBackend(t *testing.T) {
	t.Parallel()
	svc, _ := newServiceHarness(t)
	_, err := svc.AddMount(context.Background(), &lobslawv1.AddMountRequest{
		Mount: &lobslawv1.StorageMount{Label: "x", Type: "unknown-backend", Path: "/x"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown backend should be InvalidArgument; got %v", err)
	}
}

func TestRemoveMountHappyPath(t *testing.T) {
	t.Parallel()
	svc, _ := newServiceHarness(t)
	_, _ = svc.AddMount(context.Background(), &lobslawv1.AddMountRequest{
		Mount: &lobslawv1.StorageMount{Label: "x", Type: "fake", Path: "/x"},
	})
	if _, err := svc.RemoveMount(context.Background(), &lobslawv1.RemoveMountRequest{Label: "x"}); err != nil {
		t.Fatal(err)
	}
	resp, _ := svc.ListMounts(context.Background(), &lobslawv1.ListMountsRequest{})
	if len(resp.Mounts) != 0 {
		t.Errorf("after remove: %+v", resp.Mounts)
	}
}

func TestRemoveMountUnknownIsNotFound(t *testing.T) {
	t.Parallel()
	svc, _ := newServiceHarness(t)
	_, err := svc.RemoveMount(context.Background(), &lobslawv1.RemoveMountRequest{Label: "ghost"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("want NotFound; got %v", err)
	}
}

// TestReconcileMaterialisesAndRemoves — Reconcile brings the local
// Manager in line with the replicated bucket. Add a mount via raw
// PUT (bypassing AddMount) then call Reconcile and assert the
// Manager registered it. Delete the backing record and call
// Reconcile again → Manager unregisters.
func TestReconcileMaterialisesAndRemoves(t *testing.T) {
	t.Parallel()
	node, store, fsm := singleNodeRaft(t, "recon-node")
	mgr := NewManager()
	svc, _ := NewService(ServiceConfig{
		Raft: node, Store: store, FSM: fsm, Manager: mgr,
		Factories: map[string]BackendFactory{"fake": fakeFactory},
	})

	// PUT a StorageMount directly.
	putEntry := &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT,
		Id: "sharedX",
		Payload: &lobslawv1.LogEntry_StorageMount{
			StorageMount: &lobslawv1.StorageMount{
				Label: "sharedX", Type: "fake", Path: "/srv/shared",
			},
		},
	}
	data, _ := proto.Marshal(putEntry)
	res, err := node.Apply(data, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ferr, ok := res.(error); ok && ferr != nil {
		t.Fatal(ferr)
	}

	if err := svc.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, err := mgr.Resolve("sharedX"); err != nil || got != "/srv/shared" {
		t.Errorf("reconcile should register the mount; got %q err=%v", got, err)
	}

	// DELETE and reconcile — Manager should drop it.
	delEntry := &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_DELETE,
		Id: "sharedX",
		Payload: &lobslawv1.LogEntry_StorageMount{
			StorageMount: &lobslawv1.StorageMount{Label: "sharedX"},
		},
	}
	delData, _ := proto.Marshal(delEntry)
	if _, err := node.Apply(delData, 5*time.Second); err != nil {
		t.Fatal(err)
	}

	if err := svc.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Resolve("sharedX"); err != ErrNotFound {
		t.Errorf("reconcile should drop deleted mount; got %v", err)
	}
}

// TestReconcileSkipsUnknownBackend — a mount whose Type has no
// registered factory is logged + skipped, not an error. Lets a
// cluster roll out a new backend by deploying the binary first,
// then adding the mount config.
func TestReconcileSkipsUnknownBackend(t *testing.T) {
	t.Parallel()
	node, store, fsm := singleNodeRaft(t, "skip-node")
	mgr := NewManager()
	svc, _ := NewService(ServiceConfig{
		Raft: node, Store: store, FSM: fsm, Manager: mgr,
		Factories: map[string]BackendFactory{"known": fakeFactory},
	})

	putEntry := &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT,
		Id: "future",
		Payload: &lobslawv1.LogEntry_StorageMount{
			StorageMount: &lobslawv1.StorageMount{
				Label: "future", Type: "future-backend", Path: "x",
			},
		},
	}
	data, _ := proto.Marshal(putEntry)
	_, _ = node.Apply(data, 5*time.Second)

	if err := svc.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Resolve("future"); err != ErrNotFound {
		t.Errorf("unknown backend should be skipped; got %v", err)
	}
}

// Compile-time guard: the StorageMount proto's PollInterval field
// is used by the watcher machinery via Manager.Watch. This silences
// the unused warning if we ever trim back.
var _ = durationpb.New
