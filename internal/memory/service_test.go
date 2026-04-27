package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// newTestServiceStack builds a single-node Raft + Store + Service
// using the in-memory transport. Cheap (no gRPC) and deterministic,
// suitable for service-level unit tests.
func newTestServiceStack(t *testing.T) *Service {
	t.Helper()

	dataDir := t.TempDir()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dataDir, "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	fsm := NewFSM(store)

	_, inmem := raft.NewInmemTransport(raft.ServerAddress("test-node"))

	node, err := NewRaft(RaftConfig{
		NodeID:    "test-node",
		LocalAddr: "test-node",
		DataDir:   dataDir,
		Bootstrap: true,
		Transport: inmem,
	}, fsm)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = node.Shutdown()
		_ = store.Close()
	})

	if err := node.WaitForLeader(5 * time.Second); err != nil {
		t.Fatal(err)
	}

	return NewService(node, store, nil)
}

func TestServiceStoreAndRecall(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	ctx := context.Background()

	_, err := svc.Store(ctx, &lobslawv1.StoreRequest{
		Record: &lobslawv1.VectorRecord{
			Id:        "mem-1",
			Embedding: []float32{1, 0, 0},
			Text:      "hello",
			Scope:     "user:alice",
		},
	})
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	got, err := svc.Recall(ctx, &lobslawv1.RecallRequest{Id: "mem-1"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if got.Record.Text != "hello" {
		t.Errorf("text = %q, want hello", got.Record.Text)
	}
	if got.Record.Retention != lobslawv1.Retention_RETENTION_EPISODIC {
		t.Errorf("retention = %v, want episodic (default)", got.Record.Retention)
	}
	if got.Record.CreatedAt == nil {
		t.Error("CreatedAt should have been auto-populated")
	}
}

func TestServiceRecallMissing(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	_, err := svc.Recall(context.Background(), &lobslawv1.RecallRequest{Id: "nope"})
	if err == nil {
		t.Fatal("expected error")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound", st.Code())
	}
}

func TestServiceStoreRejectsInvalid(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	ctx := context.Background()

	cases := []struct {
		name string
		req  *lobslawv1.StoreRequest
	}{
		{"nil request", nil},
		{"nil record", &lobslawv1.StoreRequest{}},
		{"empty id", &lobslawv1.StoreRequest{Record: &lobslawv1.VectorRecord{Id: ""}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Store(ctx, tc.req)
			if err == nil {
				t.Fatal("expected error")
			}
			st, _ := status.FromError(err)
			if st.Code() != codes.InvalidArgument {
				t.Errorf("code = %v, want InvalidArgument", st.Code())
			}
		})
	}
}

func TestServiceEpisodicAdd(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	ctx := context.Background()

	resp, err := svc.EpisodicAdd(ctx, &lobslawv1.EpisodicAddRequest{
		Record: &lobslawv1.EpisodicRecord{
			Id:    "ep-1",
			Event: "user mentioned they like hiking",
		},
	})
	if err != nil {
		t.Fatalf("EpisodicAdd: %v", err)
	}
	if resp.Id != "ep-1" {
		t.Errorf("Id = %q, want ep-1", resp.Id)
	}

	// Verify defaults applied.
	raw, err := svc.store.Get(BucketEpisodicRecords, "ep-1")
	if err != nil {
		t.Fatal(err)
	}
	var got lobslawv1.EpisodicRecord
	if err := proto.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Importance != 5 {
		t.Errorf("Importance = %d, want 5 (default)", got.Importance)
	}
	if got.Retention != lobslawv1.Retention_RETENTION_EPISODIC {
		t.Errorf("Retention = %v, want episodic (default)", got.Retention)
	}
	if got.Timestamp == nil {
		t.Error("Timestamp should have been auto-populated")
	}
}

func TestServiceSearch(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	ctx := context.Background()

	for i, vec := range [][]float32{
		{1, 0, 0},
		{0.9, 0.1, 0},
		{0, 1, 0},
		{0, 0, 1},
	} {
		id := []string{"v-a", "v-b", "v-c", "v-d"}[i]
		_, err := svc.Store(ctx, &lobslawv1.StoreRequest{
			Record: &lobslawv1.VectorRecord{Id: id, Embedding: vec},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	resp, err := svc.Search(ctx, &lobslawv1.SearchRequest{
		Embedding: []float32{1, 0, 0},
		Limit:     2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(resp.Hits))
	}
	// Expect v-a first (exact match), v-b second.
	if resp.Hits[0].Id != "v-a" || resp.Hits[1].Id != "v-b" {
		t.Errorf("hit order = %q, %q; want v-a, v-b", resp.Hits[0].Id, resp.Hits[1].Id)
	}
}

func TestServiceSearchTextNotImplemented(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	_, err := svc.Search(context.Background(), &lobslawv1.SearchRequest{Text: "hello"})
	if err == nil {
		t.Fatal("expected error")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", st.Code())
	}
}

func TestServiceSearchEmptyEmbeddingRejected(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	_, err := svc.Search(context.Background(), &lobslawv1.SearchRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

func TestServiceAppliesEmitNoFSMError(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	ctx := context.Background()
	// Store a record with a payload we know Fails fast if FSM misbehaves.
	_, err := svc.Store(ctx, &lobslawv1.StoreRequest{
		Record: &lobslawv1.VectorRecord{Id: "x", Embedding: []float32{1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Double-store with same id should also succeed (idempotent PUT).
	if _, err := svc.Store(ctx, &lobslawv1.StoreRequest{
		Record: &lobslawv1.VectorRecord{Id: "x", Embedding: []float32{1}},
	}); err != nil {
		t.Error(err)
	}
}
