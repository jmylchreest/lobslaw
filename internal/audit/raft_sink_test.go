package audit

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// newRaftSink spins up a single-node Raft cluster and returns a
// wired RaftSink. Uses in-memory transport so tests don't touch the
// network.
func newRaftSink(t *testing.T) (*RaftSink, *memory.RaftNode, *memory.Store) {
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
	localAddr := raft.ServerAddress("audit-test")
	_, inmem := raft.NewInmemTransport(localAddr)
	node, err := memory.NewRaft(memory.RaftConfig{
		NodeID: "audit-test", LocalAddr: localAddr, DataDir: dir, Bootstrap: true, Transport: inmem,
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
	sink, err := NewRaftSink(RaftConfig{Raft: node, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	return sink, node, store
}

func TestRaftSinkAppendAndQuery(t *testing.T) {
	t.Parallel()
	s, _, _ := newRaftSink(t)
	ctx := t.Context()

	var prev string
	for i := 0; i < 3; i++ {
		e := fillEntry(i)
		e.PrevHash = prev
		if err := s.Append(ctx, e); err != nil {
			t.Fatalf("Append: %v", err)
		}
		prev = ComputeHash(e)
	}

	got, err := s.Query(ctx, types.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 entries; got %d", len(got))
	}
	if got[0].Timestamp.After(got[2].Timestamp) {
		t.Error("entries should be in ascending timestamp order")
	}
}

func TestRaftSinkQueryFiltersActor(t *testing.T) {
	t.Parallel()
	s, _, _ := newRaftSink(t)
	ctx := t.Context()

	for i, scope := range []string{"user:alice", "user:bob", "user:alice"} {
		e := fillEntry(i)
		e.ActorScope = scope
		if err := s.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Query(ctx, types.AuditFilter{ActorScope: "user:alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("want 2 alice entries; got %d", len(got))
	}
	for _, e := range got {
		if e.ActorScope != "user:alice" {
			t.Errorf("filter leaked: %+v", e)
		}
	}
}

func TestRaftSinkQueryLimit(t *testing.T) {
	t.Parallel()
	s, _, _ := newRaftSink(t)
	ctx := t.Context()
	for i := 0; i < 5; i++ {
		if err := s.Append(ctx, fillEntry(i)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Query(ctx, types.AuditFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("Limit=2 cap: got %d", len(got))
	}
}

func TestRaftSinkVerifyChainClean(t *testing.T) {
	t.Parallel()
	s, _, _ := newRaftSink(t)
	ctx := t.Context()
	var prev string
	for i := 0; i < 4; i++ {
		e := fillEntry(i)
		e.PrevHash = prev
		if err := s.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
		prev = ComputeHash(e)
	}
	res, err := s.VerifyChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Errorf("clean chain should verify OK; FirstBreakID=%q", res.FirstBreakID)
	}
	if res.EntriesChecked != 4 {
		t.Errorf("EntriesChecked = %d; want 4", res.EntriesChecked)
	}
}

// TestRaftSinkVerifyChainDetectsMismatch plants a deliberately bad
// entry whose PrevHash doesn't match its predecessor, simulating a
// tampered or corrupted bbolt row.
func TestRaftSinkVerifyChainDetectsMismatch(t *testing.T) {
	t.Parallel()
	s, _, _ := newRaftSink(t)
	ctx := t.Context()

	// First a clean entry.
	e0 := fillEntry(0)
	if err := s.Append(ctx, e0); err != nil {
		t.Fatal(err)
	}

	// Second entry with a bogus PrevHash — the chain expects
	// ComputeHash(e0), but we plant random bytes instead.
	bad := fillEntry(1)
	bad.PrevHash = "deadbeefdeadbeefdeadbeefdeadbeef"
	if err := s.Append(ctx, bad); err != nil {
		t.Fatal(err)
	}

	res, err := s.VerifyChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Error("chain with bad PrevHash must NOT verify OK")
	}
	if res.FirstBreakID != bad.ID {
		t.Errorf("FirstBreakID=%q; want %q", res.FirstBreakID, bad.ID)
	}
}

func TestRaftSinkName(t *testing.T) {
	t.Parallel()
	s, _, _ := newRaftSink(t)
	if s.Name() != "raft" {
		t.Errorf("Name = %q; want raft", s.Name())
	}
}

func TestRaftSinkConfigValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewRaftSink(RaftConfig{}); err == nil {
		t.Error("empty config should fail")
	}
	if _, err := NewRaftSink(RaftConfig{Raft: nil, Store: &memory.Store{}}); err == nil {
		t.Error("missing Raft should fail")
	}
}

func TestTypedProtoRoundTrip(t *testing.T) {
	t.Parallel()
	in := types.AuditEntry{
		ID:         "01HABC",
		Timestamp:  time.Unix(1_700_000_000, 123).UTC(),
		ActorScope: "user:alice",
		Action:     "memory:write",
		Target:     "notebook",
		Argv:       []string{"--tag", "personal"},
		PolicyRule: "allow-memory",
		Effect:     types.EffectAllow,
		ResultHash: "abc123",
		PrevHash:   "prev-hash",
	}
	out := protoToTyped(typedToProto(in))
	if out.ID != in.ID || out.ActorScope != in.ActorScope || out.Action != in.Action {
		t.Errorf("roundtrip dropped scalar fields: %+v", out)
	}
	if !out.Timestamp.Equal(in.Timestamp) {
		t.Errorf("roundtrip timestamp: got %v; want %v", out.Timestamp, in.Timestamp)
	}
	if len(out.Argv) != 2 || out.Argv[0] != "--tag" {
		t.Errorf("roundtrip argv: %+v", out.Argv)
	}
	if out.Effect != in.Effect {
		t.Errorf("roundtrip effect: %q; want %q", out.Effect, in.Effect)
	}
}
