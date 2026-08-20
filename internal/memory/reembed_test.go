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
