package memory

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// stubEmbedder returns a fixed vector and a fixed identity.
type stubEmbedder struct{ model string }

func (s stubEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{1, 0, 0, 0}, nil
}
func (s stubEmbedder) Model() string { return s.model }

func addEpisodic(t *testing.T, svc *Service, id, text string) {
	t.Helper()
	if _, err := svc.EpisodicAdd(context.Background(), &lobslawv1.EpisodicAddRequest{
		Record: &lobslawv1.EpisodicRecord{
			Id: id, Event: text, Context: text, Importance: 5,
			Timestamp: timestamppb.Now(),
			Owner:     "user:alice", Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE,
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func countVectors(t *testing.T, svc *Service) (total, stamped int, models map[string]int) {
	t.Helper()
	models = map[string]int{}
	if _, err := svc.store.ForEachDecryptable(BucketVectorRecords, func(_ string, raw []byte) error {
		var v lobslawv1.VectorRecord
		if err := proto.Unmarshal(raw, &v); err != nil {
			return nil
		}
		total++
		if v.EmbeddingModel != "" {
			stamped++
			models[v.EmbeddingModel]++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return total, stamped, models
}

// DREAM'S CONSOLIDATIONS MUST SURVIVE.
//
// A consolidation is a vector record too, and it lists every episodic
// record it merged. An earlier version keyed the "existing vector for
// this record" map on each source in turn, so re-embedding any one
// member deleted the consolidation containing it — destroying dream's
// output on every run, silently, since nothing counts consolidations.
//
// Caught on a real store: a second pass reported 77 superseded vectors
// for 57 records, and the extra 20 were consolidations.
func TestReembedDoesNotDestroyConsolidations(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	svc.SetEmbedder(stubEmbedder{model: "new-model"})

	addEpisodic(t, svc, "e1", "the sourdough starter is fed on tuesdays")
	addEpisodic(t, svc, "e2", "the user is based in Yorkshire")

	// A consolidation over BOTH, as dream writes them.
	consolidation := &lobslawv1.VectorRecord{
		Id: "dream-1", Text: "a summary of both", SourceIds: []string{"e1", "e2"},
		Owner: "user:alice", Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE,
	}
	raw, err := proto.Marshal(consolidation)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.store.Put(BucketVectorRecords, "dream-1", raw); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Reembed(context.Background(), &lobslawv1.ReembedRequest{}); err != nil {
		t.Fatalf("Reembed: %v", err)
	}

	if _, err := svc.store.Get(BucketVectorRecords, "dream-1"); err != nil {
		t.Fatal("the consolidation was deleted; dream's output does not survive a re-embed")
	}
}

// Every record ends up with a vector carrying the CURRENT model, and
// vectors from the previous one are gone — which is the whole point,
// since one of them is enough to make the next boot refuse.
func TestReembedLeavesOnlyTheCurrentModel(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)

	// A corpus embedded by the old model.
	svc.SetEmbedder(stubEmbedder{model: "old-model"})
	addEpisodic(t, svc, "e1", "one")
	addEpisodic(t, svc, "e2", "two")
	if _, err := svc.Reembed(context.Background(), &lobslawv1.ReembedRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, _, models := countVectors(t, svc); models["old-model"] == 0 {
		t.Fatal("setup wrote no old-model vectors, so the migration below proves nothing")
	}

	svc.SetEmbedder(stubEmbedder{model: "new-model"})
	res, err := svc.Reembed(context.Background(), &lobslawv1.ReembedRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.GetReembedded() != 2 {
		t.Errorf("re-embedded %d records, want 2", res.GetReembedded())
	}
	_, _, models := countVectors(t, svc)
	if models["old-model"] != 0 {
		t.Errorf("%d vector(s) still carry the old model; the next boot would be refused", models["old-model"])
	}
	if models["new-model"] != 2 {
		t.Errorf("%d vector(s) carry the new model, want 2", models["new-model"])
	}
	// And the guard agrees.
	if err := CheckEmbeddingModel(svc.store, "new-model"); err != nil {
		t.Errorf("after re-embedding, the boot guard still refuses: %v", err)
	}
}

// Without an embedder this must refuse clearly rather than silently
// doing nothing — a re-embed that reports success and writes no
// vectors is worse than an error.
func TestReembedWithoutAnEmbedderRefuses(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	addEpisodic(t, svc, "e1", "one")
	if _, err := svc.Reembed(context.Background(), &lobslawv1.ReembedRequest{}); err == nil {
		t.Error("Reembed with no embedder reported success")
	}
}

// AN UNSTAMPED VECTOR OF THE WRONG WIDTH IS UNREACHABLE BOTH WAYS.
//
// vectorSearch skips any vector whose width differs from the query's —
// it has no choice, the dot product is undefined — and logs "skipped
// records with mismatched embedding width". The sweep, meanwhile,
// deliberately spared unstamped vectors on the grounds that removing
// one would be "destroying something on a guess".
//
// Between them nothing ever fixed such a vector. It could not be
// searched and could not be repaired, and the only sign was a WARN
// naming a count. Found exactly that way on the test rig: a re-embed
// reported success while the next search still skipped four records.
//
// Width is not a guess. A vector of another width is provably not
// comparable to this model's, which is why search already refuses it.
// Deleting it destroys nothing that was reachable, and the record it
// came from is re-embedded in the same pass.
func TestReembedRemovesUnstampedVectorsOfAnotherWidth(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	svc.SetEmbedder(stubEmbedder{model: "new-model"}) // 4-wide

	addEpisodic(t, svc, "e1", "the spare keys are in the top-left cupboard")

	// Unstamped, and SIX wide: written before the model field existed.
	old := &lobslawv1.VectorRecord{
		Id: "v-old", Text: "the spare keys are in the top-left cupboard",
		Embedding: []float32{1, 0, 0, 0, 0, 0}, SourceIds: []string{"e1"},
		Owner: "user:alice", Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE,
	}
	raw, err := proto.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.store.Put(BucketVectorRecords, "v-old", raw); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Reembed(context.Background(), &lobslawv1.ReembedRequest{}); err != nil {
		t.Fatalf("Reembed: %v", err)
	}

	// Nothing of another width may remain, or search still skips it and
	// the operator is told a repair succeeded that did not.
	if _, err := svc.store.ForEachDecryptable(BucketVectorRecords, func(k string, raw []byte) error {
		var v lobslawv1.VectorRecord
		if err := proto.Unmarshal(raw, &v); err != nil {
			return nil
		}
		if len(v.Embedding) != 4 {
			t.Errorf("vector %q is %d wide after a re-embed onto a 4-wide model; "+
				"search will skip it and no repair can reach it", k, len(v.Embedding))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// And the record itself must still be searchable — the point was to
	// repair it, not to delete its only vector.
	var found bool
	if _, err := svc.store.ForEachDecryptable(BucketVectorRecords, func(_ string, raw []byte) error {
		var v lobslawv1.VectorRecord
		if err := proto.Unmarshal(raw, &v); err != nil {
			return nil
		}
		if len(v.SourceIds) == 1 && v.SourceIds[0] == "e1" {
			found = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Error("e1 has no vector at all after the re-embed")
	}
}

// THE CASE THAT WAS IN NEITHER MAP.
//
// A single-source vector of the wrong width is deleted as its record's
// superseded one. A stamped vector of the wrong model is swept. A
// CONSOLIDATION of the wrong width was neither: it has many sources,
// so no record claims it, and if it predates the model field there is
// no stamp to disagree with.
//
// So it stayed, and every search paid for it — skipped, counted in a
// WARN, and reported to the operator as a repair that succeeded.
//
// It is repaired rather than removed. Deleting would also stop the
// skips, and would throw away a summary that can be re-embedded from
// the text stored beside it.
func TestReembedRepairsConsolidationsOfAnotherWidth(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	svc.SetEmbedder(stubEmbedder{model: "new-model"}) // 4-wide

	addEpisodic(t, svc, "e1", "the sourdough starter is fed on tuesdays")
	addEpisodic(t, svc, "e2", "the user is based in Yorkshire")

	stale := &lobslawv1.VectorRecord{
		Id: "dream-old", Text: "a summary of both", SourceIds: []string{"e1", "e2"},
		Embedding: []float32{1, 0, 0, 0, 0, 0}, // SIX wide, unstamped
		Owner:     "user:alice", Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE,
	}
	raw, err := proto.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.store.Put(BucketVectorRecords, "dream-old", raw); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Reembed(context.Background(), &lobslawv1.ReembedRequest{}); err != nil {
		t.Fatalf("Reembed: %v", err)
	}

	got, err := svc.store.Get(BucketVectorRecords, "dream-old")
	if err != nil {
		t.Fatal("the consolidation was deleted; it could have been re-embedded")
	}
	var v lobslawv1.VectorRecord
	if err := proto.Unmarshal(got, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Embedding) != 4 {
		t.Errorf("consolidation is %d wide after a re-embed onto a 4-wide model; "+
			"search will skip it forever", len(v.Embedding))
	}
	if v.Text != "a summary of both" || len(v.SourceIds) != 2 {
		t.Errorf("dream's output was rewritten, not just re-embedded: text=%q sources=%v",
			v.Text, v.SourceIds)
	}
}

// A CONSOLIDATION CAN BE RE-EMBEDDED WITHOUT REGENERATING IT.
//
// Reembed used to leave every consolidation alone, on the reasoning
// that its text is a summary this package cannot produce and dream
// owns. That is true of the TEXT and false of the VECTOR: the summary
// is already stored on the record, and embedding text we have needs
// nothing from dream.
//
// It matters because dream writes a consolidation with a NIL embedding
// whenever no embedder is configured — Summarize returns one — so a
// cluster that gained an embedder later had summaries that no search
// could ever reach. On the rig that was four of them, and each one
// costs every search a skip and the operator a warning, while the
// repair reports success.
func TestReembedGivesConsolidationsAVector(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	svc.SetEmbedder(stubEmbedder{model: "new-model"}) // 4-wide

	addEpisodic(t, svc, "e1", "the sourdough starter is fed on tuesdays")
	addEpisodic(t, svc, "e2", "the user is based in Yorkshire")

	// As dream writes one with no embedder configured: text, no vector.
	consolidation := &lobslawv1.VectorRecord{
		Id: "dream-1", Text: "a summary of both", SourceIds: []string{"e1", "e2"},
		Owner: "user:alice", Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE,
	}
	raw, err := proto.Marshal(consolidation)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.store.Put(BucketVectorRecords, "dream-1", raw); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Reembed(context.Background(), &lobslawv1.ReembedRequest{}); err != nil {
		t.Fatalf("Reembed: %v", err)
	}

	got, err := svc.store.Get(BucketVectorRecords, "dream-1")
	if err != nil {
		t.Fatal("the consolidation was deleted rather than re-embedded")
	}
	var v lobslawv1.VectorRecord
	if err := proto.Unmarshal(got, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Embedding) != 4 {
		t.Errorf("consolidation embedding is %d wide, want 4 — no search can reach it", len(v.Embedding))
	}
	if v.EmbeddingModel != "new-model" {
		t.Errorf("embedding_model = %q, want \"new-model\"", v.EmbeddingModel)
	}
	// The summary and its provenance must survive verbatim: this pass
	// re-embeds dream's output, it does not rewrite it.
	if v.Text != "a summary of both" {
		t.Errorf("text = %q, want the original summary", v.Text)
	}
	if len(v.SourceIds) != 2 {
		t.Errorf("source_ids = %v, want both sources", v.SourceIds)
	}
}
