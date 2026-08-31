package tools

import (
	"context"
	"testing"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"
)

// A memory the model writes has to be one it can read back.
//
// visibility.go says an unowned record "is readable by nobody but
// Everyone()", and that being invisible "is how it surfaces rather
// than how it hides" — the surfacing being that an unowned record
// means a bug upstream. memory_write was that bug: it built an
// EpisodicRecord with no Owner, so every memory the model was asked to
// keep became unreadable by passive recall and by memory_search the
// moment it was written.
//
// The ingest path refuses an ownerless turn outright
// (episodic_ingest.go). The tool path had no equivalent, so the same
// mistake was loud in one place and silent in the other.
func TestAMemoryTheModelWritesIsOwnedByTheCaller(t *testing.T) {
	t.Parallel()

	applier := &fakeApplier{}
	h := newMemoryWriteHandler(applier, nil)

	ctx := turn.WithIdentity(context.Background(), turn.Identity{
		UserID:    "tg-@alice",
		Principal: identity.User("alice"),
		Channel:   "telegram",
		ChannelID: "-100",
	})
	if _, code, err := h(ctx, map[string]string{
		"event":   "moved house",
		"context": "The user moved to Leeds in August.",
	}); err != nil || code != 0 {
		t.Fatalf("memory_write: code=%d err=%v", code, err)
	}

	if len(applier.entries) == 0 {
		t.Fatal("memory_write applied nothing")
	}
	rec := applier.entries[len(applier.entries)-1].GetEpisodicRecord()
	if rec == nil {
		t.Fatal("the applied entry carries no episodic record")
	}

	if rec.Owner == "" {
		t.Error("the record has no owner; nothing but Everyone() can ever read it, " +
			"so the memory is written and immediately unreachable")
	}
	if want := "user:alice"; rec.Owner != want {
		t.Errorf("Owner = %q, want %q (the caller's principal, not their channel id)", rec.Owner, want)
	}
	if rec.Visibility != lobslawv1.Visibility_VISIBILITY_PRIVATE {
		t.Errorf("Visibility = %v, want PRIVATE — an owned record needs a visibility to match on",
			rec.Visibility)
	}
	if rec.SessionRef == "" {
		t.Error("no SessionRef; the memory cannot be found by the conversation it came from")
	}
}

// An anonymous turn still has to produce a readable record or a clear
// refusal — never a write that silently goes nowhere.
func TestAnUnidentifiedWriteDoesNotSilentlyVanish(t *testing.T) {
	t.Parallel()

	applier := &fakeApplier{}
	h := newMemoryWriteHandler(applier, nil)

	out, code, err := h(context.Background(), map[string]string{
		"event":   "a fact",
		"context": "with no identity on the turn",
	})
	if err == nil && code == 0 {
		rec := applier.entries[len(applier.entries)-1].GetEpisodicRecord()
		if rec.Owner == "" {
			t.Errorf("an ownerless write reported success (%s); it stored a record "+
				"nobody can read and told the model it had remembered something", out)
		}
	}
}

// A correction inherits what made the original findable.
//
// It used to hardcode importance 5, replace the tag list with just the
// corrects marker, and force long-term retention — so correcting an
// importance-9 memory demoted it, and correcting a tagged one dropped
// every topic tag it was findable by. A correction that makes a memory
// harder to find has undone more than it fixed.
func TestACorrectionKeepsWhatMadeTheOriginalFindable(t *testing.T) {
	t.Parallel()

	store := newMemoryStoreForTest(t)
	prior := &lobslawv1.EpisodicRecord{
		Id:         "old-1",
		Event:      "lives in Manchester",
		Importance: 9,
		Tags:       []string{"topic:home", "source:user"},
		Retention:  lobslawv1.Retention_RETENTION_SESSION,
		Owner:      "user:alice",
		Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE,
	}
	seedEpisodic(t, store, prior)

	applier := &fakeApplier{}
	h := newMemoryCorrectHandler(store, applier, &noopForgetter{}, nil, nil)

	ctx := turn.WithIdentity(context.Background(), turn.Identity{
		Principal: identity.User("alice"), Channel: "telegram", ChannelID: "-100",
	})
	if _, code, err := h(ctx, map[string]string{
		"id": "old-1", "new_event": "lives in Leeds",
	}); err != nil || code != 0 {
		t.Fatalf("memory_correct: code=%d err=%v", code, err)
	}

	rec := applier.entries[0].GetEpisodicRecord()
	if rec.Importance != 9 {
		t.Errorf("Importance = %d, want 9 — the correction demoted the memory it replaced", rec.Importance)
	}
	if rec.Retention != lobslawv1.Retention_RETENTION_SESSION {
		t.Errorf("Retention = %v, want SESSION — the correction promoted a deliberately scoped memory", rec.Retention)
	}
	var haveTopic, haveCorrects bool
	for _, tg := range rec.Tags {
		switch tg {
		case "topic:home":
			haveTopic = true
		case "corrects:old-1":
			haveCorrects = true
		}
	}
	if !haveTopic {
		t.Errorf("tags = %v; the original's topic tags were dropped, so the corrected memory is harder to find", rec.Tags)
	}
	if !haveCorrects {
		t.Errorf("tags = %v; the corrects marker is missing, so the audit trail is broken", rec.Tags)
	}
	if rec.Owner != "user:alice" {
		t.Errorf("Owner = %q; an unowned correction replaces a readable memory with an invisible one", rec.Owner)
	}
}

// noopForgetter stands in for the forget step, which this test is not
// about.
type noopForgetter struct{}

func (noopForgetter) Forget(context.Context, *lobslawv1.ForgetRequest) (*lobslawv1.ForgetResponse, error) {
	return &lobslawv1.ForgetResponse{RecordsRemoved: 1}, nil
}

// stubEmbedder returns a fixed vector so a test can assert the index
// entry without an embedding service.
type stubEmbedder struct{ model string }

func (s stubEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{0.1, 0.2}, nil
}
func (s stubEmbedder) EmbedQuery(ctx context.Context, t string) ([]float32, error) {
	return s.Embed(ctx, t)
}
func (s stubEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{0.1, 0.2}
	}
	return out, nil
}
func (s stubEmbedder) Dimensions() int { return 2 }
func (s stubEmbedder) Model() string   { return s.model }

// A written memory has to be findable the way memories are usually
// found.
//
// Passive recall prefers the vector path and only falls back to
// substring matching, so a memory with no vector was reachable only by
// the words it happened to reuse. Episodic ingest has always written
// the pair; the tool path never did, which made "remember this"
// produce a weaker memory than the same fact arriving in conversation.
func TestAWrittenMemoryIsIndexedForVectorRecall(t *testing.T) {
	t.Parallel()

	applier := &fakeApplier{}
	h := newMemoryWriteHandler(applier, stubEmbedder{model: "test-embedder"})

	ctx := turn.WithIdentity(context.Background(), turn.Identity{
		Principal: identity.User("alice"), Channel: "telegram", ChannelID: "-100",
	})
	if _, code, err := h(ctx, map[string]string{
		"event": "moved house", "context": "The user moved to Leeds in August.",
	}); err != nil || code != 0 {
		t.Fatalf("memory_write: code=%d err=%v", code, err)
	}

	var epi *lobslawv1.EpisodicRecord
	var vec *lobslawv1.VectorRecord
	for _, e := range applier.entries {
		if r := e.GetEpisodicRecord(); r != nil {
			epi = r
		}
		if v := e.GetVectorRecord(); v != nil {
			vec = v
		}
	}
	if epi == nil {
		t.Fatal("no episodic record was written")
	}
	if vec == nil {
		t.Fatal("no vector record was written; the memory is reachable only by lexical fallback")
	}

	// The vector must carry the episodic record's ownership. Search
	// reads vectors, so an unowned vector over owned text is the same
	// leak wearing a different hat.
	if vec.Owner != epi.Owner {
		t.Errorf("vector owner = %q, episodic owner = %q", vec.Owner, epi.Owner)
	}
	if vec.Visibility != epi.Visibility {
		t.Errorf("vector visibility = %v, episodic = %v", vec.Visibility, epi.Visibility)
	}
	if vec.SessionRef != epi.SessionRef {
		t.Errorf("vector session = %q, episodic = %q; it is invisible in the conversation that produced it",
			vec.SessionRef, epi.SessionRef)
	}
	if len(vec.SourceIds) != 1 || vec.SourceIds[0] != epi.Id {
		t.Errorf("vector SourceIds = %v, want [%s]", vec.SourceIds, epi.Id)
	}
	if vec.EmbeddingModel != "test-embedder" {
		t.Errorf("EmbeddingModel = %q; a vector whose model is unknown cannot be re-embedded safely", vec.EmbeddingModel)
	}
}

// No embedder is not an error. The episodic record is the memory; the
// vector is an index over it, and a node with no embeddings configured
// still has to be able to remember something.
func TestAWriteWithNoEmbedderStillStoresTheMemory(t *testing.T) {
	t.Parallel()

	applier := &fakeApplier{}
	h := newMemoryWriteHandler(applier, nil)
	ctx := turn.WithIdentity(context.Background(), turn.Identity{Principal: identity.User("alice")})

	if _, code, err := h(ctx, map[string]string{"event": "a fact"}); err != nil || code != 0 {
		t.Fatalf("memory_write: code=%d err=%v", code, err)
	}
	if applier.entries[0].GetEpisodicRecord() == nil {
		t.Error("the episodic record was not written")
	}
}
