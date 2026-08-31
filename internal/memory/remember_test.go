package memory

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"
)

type captureApplier struct{ entries []*lobslawv1.LogEntry }

func (c *captureApplier) Apply(data []byte, _ time.Duration) (any, error) {
	var e lobslawv1.LogEntry
	if err := proto.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	c.entries = append(c.entries, &e)
	return nil, nil
}

type fixedEmbedder struct{}

func (fixedEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{0.1, 0.2}, nil
}
func (fixedEmbedder) Model() string { return "test-model" }

func ownedCtx() context.Context {
	return turn.WithIdentity(context.Background(), turn.Identity{
		Principal: identity.User("alice"), Channel: "telegram", ChannelID: "-100",
	})
}

// Everything a complete memory needs, in one place.
//
// There used to be three doors — conversation ingest, Service.EpisodicAdd
// and the memory_* tools — and only ingest was complete. The other two
// each assembled a record by hand and applied it, and each missed the
// same two things: ownership, without which nobody can read the record,
// and the vector, without which nothing can find it by meaning.
func TestRememberStampsOwnershipAndIndexes(t *testing.T) {
	t.Parallel()

	raft := &captureApplier{}
	id, err := Remember(ownedCtx(), raft, fixedEmbedder{}, 0, &lobslawv1.EpisodicRecord{
		Event: "moved house", Context: "The user moved to Leeds.",
	})
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if id == "" {
		t.Error("no id returned")
	}

	var epi *lobslawv1.EpisodicRecord
	var vec *lobslawv1.VectorRecord
	for _, e := range raft.entries {
		if r := e.GetEpisodicRecord(); r != nil {
			epi = r
		}
		if v := e.GetVectorRecord(); v != nil {
			vec = v
		}
	}
	if epi == nil {
		t.Fatal("no episodic record written")
	}
	if vec == nil {
		t.Fatal("no vector written; the memory is findable only by lexical fallback")
	}
	if epi.Owner != "user:alice" {
		t.Errorf("Owner = %q, want user:alice — stamped from the turn", epi.Owner)
	}
	if epi.Visibility != lobslawv1.Visibility_VISIBILITY_PRIVATE {
		t.Errorf("Visibility = %v, want PRIVATE", epi.Visibility)
	}
	if epi.SessionRef != "telegram:-100" {
		t.Errorf("SessionRef = %q, want telegram:-100", epi.SessionRef)
	}
	if vec.Owner != epi.Owner || vec.Visibility != epi.Visibility || vec.SessionRef != epi.SessionRef {
		t.Error("the vector does not carry the record's ownership; search reads vectors, " +
			"so that is the same leak wearing a different hat")
	}
}

// A caller that knows better keeps its own answer. Ingest resolves the
// turn's principal before calling in, and must not have it overwritten.
func TestRememberDoesNotOverwriteWhatTheCallerSet(t *testing.T) {
	t.Parallel()

	raft := &captureApplier{}
	if _, err := Remember(ownedCtx(), raft, nil, 0, &lobslawv1.EpisodicRecord{
		Event: "x", Owner: "user:bob", SessionRef: "slack:C1",
	}); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	got := raft.entries[0].GetEpisodicRecord()
	if got.Owner != "user:bob" {
		t.Errorf("Owner = %q; the caller's own answer was overwritten", got.Owner)
	}
	if got.SessionRef != "slack:C1" {
		t.Errorf("SessionRef = %q; the caller's own answer was overwritten", got.SessionRef)
	}
}

// Refused, not stored. A record nobody owns reports success and loses
// the memory, which is worse than failing where somebody can see it.
func TestRememberRefusesARecordNobodyCouldRead(t *testing.T) {
	t.Parallel()

	raft := &captureApplier{}
	if _, err := Remember(context.Background(), raft, nil, 0, &lobslawv1.EpisodicRecord{Event: "x"}); err == nil {
		t.Error("an ownerless write succeeded; it stored something nobody can read")
	}
	if len(raft.entries) != 0 {
		t.Errorf("%d entries were applied despite the refusal", len(raft.entries))
	}
}

// No embedder is not an error: the episodic record is the memory, the
// vector is an index over it, and a node without embeddings still
// remembers.
func TestRememberWithoutAnEmbedderStillStores(t *testing.T) {
	t.Parallel()

	raft := &captureApplier{}
	if _, err := Remember(ownedCtx(), raft, nil, 0, &lobslawv1.EpisodicRecord{Event: "x"}); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if len(raft.entries) != 1 || raft.entries[0].GetEpisodicRecord() == nil {
		t.Error("the episodic record was not written")
	}
}
