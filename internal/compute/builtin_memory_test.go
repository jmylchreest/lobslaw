package compute

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// fakeApplier captures LogEntry payloads written via memory_write
// so tests can assert the bucket+content without spinning a full
// Raft cluster.
type fakeApplier struct {
	entries []*lobslawv1.LogEntry
	err     error
}

func (f *fakeApplier) Apply(data []byte, _ time.Duration) (any, error) {
	if f.err != nil {
		return f.err, nil
	}
	var entry lobslawv1.LogEntry
	if err := proto.Unmarshal(data, &entry); err != nil {
		return err, nil
	}
	f.entries = append(f.entries, &entry)
	return nil, nil
}

func newMemoryStoreForTest(t *testing.T) *memory.Store {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(t.TempDir(), "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// seedEpisodic writes a record directly via the store (bypassing
// Raft) so tests don't need a full cluster for search-only cases.
func seedEpisodic(t *testing.T, store *memory.Store, rec *lobslawv1.EpisodicRecord) {
	t.Helper()
	raw, err := proto.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(memory.BucketEpisodicRecords, rec.Id, raw); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterMemoryBuiltinsRequiresDeps(t *testing.T) {
	t.Parallel()
	b := NewBuiltins()
	if err := RegisterMemoryBuiltins(b, MemoryConfig{}); err == nil {
		t.Error("missing Store + Raft should fail register")
	}
}

func TestMemorySearchMatchesEventAndContext(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	seedEpisodic(t, store, &lobslawv1.EpisodicRecord{
		Id: "a", Event: "user prefers strong coffee",
		Context:    "Discussed brewing methods.",
		Importance: 7, Timestamp: timestamppb.Now(),
	})
	seedEpisodic(t, store, &lobslawv1.EpisodicRecord{
		Id: "b", Event: "bought new keyboard",
		Context:    "Mentioned the coffee stains on the old one.",
		Importance: 3, Timestamp: timestamppb.Now(),
	})
	seedEpisodic(t, store, &lobslawv1.EpisodicRecord{
		Id: "c", Event: "went running",
		Context:    "5k in the park",
		Importance: 4, Timestamp: timestamppb.Now(),
	})

	b := NewBuiltins()
	_ = RegisterMemoryBuiltins(b, MemoryConfig{Store: store, Raft: &fakeApplier{}})
	fn, _ := b.Get("memory_search")
	out, exit, err := fn(context.Background(), map[string]string{"query": "coffee"})
	if err != nil {
		t.Fatal(err)
	}
	if exit != 0 {
		t.Errorf("exit = %d", exit)
	}
	var payload struct {
		Query   string           `json:"query"`
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Results) != 2 {
		t.Fatalf("want 2 matches; got %d: %+v", len(payload.Results), payload.Results)
	}
	// Higher importance should sort first.
	if payload.Results[0]["id"] != "a" {
		t.Errorf("first result id = %v; want a (importance-sorted)", payload.Results[0]["id"])
	}
}

func TestMemorySearchTagFilter(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	seedEpisodic(t, store, &lobslawv1.EpisodicRecord{
		Id: "a", Event: "alpha thing", Importance: 5,
		Tags: []string{"work"}, Timestamp: timestamppb.Now(),
	})
	seedEpisodic(t, store, &lobslawv1.EpisodicRecord{
		Id: "b", Event: "alpha other thing", Importance: 5,
		Tags: []string{"personal"}, Timestamp: timestamppb.Now(),
	})
	b := NewBuiltins()
	_ = RegisterMemoryBuiltins(b, MemoryConfig{Store: store, Raft: &fakeApplier{}})
	fn, _ := b.Get("memory_search")
	out, _, _ := fn(context.Background(), map[string]string{"query": "alpha", "tag": "work"})
	var payload struct {
		Results []map[string]any `json:"results"`
	}
	_ = json.Unmarshal(out, &payload)
	if len(payload.Results) != 1 || payload.Results[0]["id"] != "a" {
		t.Errorf("tag filter failed: %+v", payload.Results)
	}
}

func TestMemoryWriteCommitsViaRaft(t *testing.T) {
	t.Parallel()
	applier := &fakeApplier{}
	b := NewBuiltins()
	_ = RegisterMemoryBuiltins(b, MemoryConfig{Store: newMemoryStoreForTest(t), Raft: applier})
	fn, _ := b.Get("memory_write")
	out, exit, err := fn(context.Background(), map[string]string{
		"event":      "user likes dark-roast coffee",
		"context":    "Mentioned preferring French roast specifically.",
		"importance": "8",
		"tags":       `["preference","food"]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if exit != 0 {
		t.Errorf("exit = %d", exit)
	}
	if len(applier.entries) != 1 {
		t.Fatalf("raft Apply called %d times; want 1", len(applier.entries))
	}
	le := applier.entries[0]
	if le.Op != lobslawv1.LogOp_LOG_OP_PUT {
		t.Errorf("op = %v; want LOG_OP_PUT", le.Op)
	}
	epi, ok := le.Payload.(*lobslawv1.LogEntry_EpisodicRecord)
	if !ok {
		t.Fatalf("payload type = %T", le.Payload)
	}
	if epi.EpisodicRecord.Importance != 8 {
		t.Errorf("importance = %d", epi.EpisodicRecord.Importance)
	}
	if len(epi.EpisodicRecord.Tags) != 2 {
		t.Errorf("tags = %v", epi.EpisodicRecord.Tags)
	}
	var resp map[string]any
	_ = json.Unmarshal(out, &resp)
	if resp["id"] == "" || resp["id"] == nil {
		t.Error("response should include the generated id")
	}
}

func TestMemoryWriteRejectsEmptyEvent(t *testing.T) {
	t.Parallel()
	b := NewBuiltins()
	_ = RegisterMemoryBuiltins(b, MemoryConfig{Store: newMemoryStoreForTest(t), Raft: &fakeApplier{}})
	fn, _ := b.Get("memory_write")
	_, exit, err := fn(context.Background(), map[string]string{})
	if err == nil || exit == 0 {
		t.Error("empty event should fail")
	}
}

func TestMemoryWriteSurfacesRaftError(t *testing.T) {
	t.Parallel()
	applier := &fakeApplier{err: errors.New("no quorum")}
	b := NewBuiltins()
	_ = RegisterMemoryBuiltins(b, MemoryConfig{Store: newMemoryStoreForTest(t), Raft: applier})
	fn, _ := b.Get("memory_write")
	_, _, err := fn(context.Background(), map[string]string{"event": "x"})
	if err == nil {
		t.Error("raft error should propagate")
	}
}
