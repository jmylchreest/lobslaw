package compute

import (
	"context"
	"strings"
	"testing"
)

// THE RELEVANCE FLOOR.
//
// Recall was bounded by how MANY records it would inject and how many
// tokens they could occupy, and by nothing at all about whether they
// had anything to do with the turn. Vector search returns its K
// nearest neighbours, not its near ones, so on a small corpus a
// message with no topic — "Hey you there?" — retrieved whatever
// happened to exist and presented it as relevant context.
//
// These tests pin the mechanism with hand-built vectors, so they run
// everywhere. The VALUE of the shipped default is a separate question
// answered against the real checkpoint in
// context_engine_calibration_test.go.

// angledEmbedder returns a query vector at a chosen cosine to the
// stored records, which all sit at {1, 0}. Building the score directly
// keeps these tests about the filter rather than about a model.
type angledEmbedder struct{ query []float32 }

func (a angledEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{1, 0}, nil
}

func (a angledEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	return a.query, nil
}

func (a angledEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func (angledEmbedder) Dimensions() int { return 2 }
func (angledEmbedder) Model() string   { return "angled-test-embedder" }

func TestRecallFloorAdmitsAndRejectsByScore(t *testing.T) {
	t.Parallel()
	// A query 60 degrees off the record scores cos(60) = 0.5; one at
	// 84 degrees scores about 0.105.
	near := []float32{0.5, 0.866}
	far := []float32{0.105, 0.994}

	for _, tc := range []struct {
		name  string
		query []float32
		floor float32
		want  bool
	}{
		{"above the floor is recalled", near, 0.25, true},
		{"below the floor is withheld", far, 0.25, false},
		{"a floor under the score admits it", far, 0.05, true},
		{"a negative floor disables the gate", far, -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newMemoryStoreForTest(t)
			seedRecallable(t, store, "e1", "the sourdough starter is fed on tuesdays", nil)

			e := NewContextEngine(ContextEngineConfig{
				Store:            store,
				Embedder:         angledEmbedder{query: tc.query},
				MinSemanticScore: tc.floor,
				// Off, so a semantic rejection cannot be quietly
				// rescued by token overlap and read as a pass.
				MinLexicalScore: 2,
			})
			got := e.Assemble(operatorTurn(context.Background()), "unrelated greeting")
			if recalled := strings.Contains(got.Rendered(), "sourdough"); recalled != tc.want {
				t.Errorf("recalled=%v, want %v (floor %.2f)\n%s",
					recalled, tc.want, tc.floor, got.Rendered())
			}
		})
	}
}

// THE TRAP THIS FEATURE MOST EASILY FALLS INTO.
//
// recall() treats zero semantic hits as a reason to try the lexical
// path, deliberately and with a long comment: zero hits is how an
// unsearchable corpus presents — every record predating the embedder,
// or every vector skipped after a model width change — and silently
// recalling nothing forever is the worse failure.
//
// But a floor that rejects every candidate ALSO produces zero hits.
// Left alone, the fallback then re-admits the same records under a
// weaker test, and the floor becomes decoration: it filters, something
// else immediately undoes it, and every test that only checks "is the
// floor applied" still passes.
//
// So the two cases have to stay distinguishable. A search that found
// candidates and rejected them has answered the question.
func TestFlooredSemanticRecallDoesNotFallBackToLexical(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	// Worded so the query's terms are all present: on the lexical path
	// this record scores a perfect 1.0, so if the fallback runs at all
	// it will certainly recall it.
	seedRecallable(t, store, "e1", "the sourdough starter is fed on tuesdays", nil)

	e := NewContextEngine(ContextEngineConfig{
		Store: store,
		// 84 degrees off: a candidate is found, and it is far away.
		Embedder:         angledEmbedder{query: []float32{0.105, 0.994}},
		MinSemanticScore: 0.25,
	})
	got := e.Assemble(operatorTurn(context.Background()), "sourdough starter fed tuesdays").Rendered()
	if strings.Contains(got, "sourdough") {
		t.Errorf("a record rejected by the semantic floor came back through the lexical fallback, "+
			"which makes the floor inert:\n%s", got)
	}
}

// The other half of that contract: a corpus with no vectors at all
// must still reach the lexical path. This is the case the fallback
// exists for, and it is easy to break while fixing the case above.
func TestUnsearchableCorpusStillFallsBackToLexical(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	// Episodic record, no paired vector — a node that ran before
	// embeddings were configured.
	seedEpisodicOnly(t, store, "e1", "the sourdough starter is fed on tuesdays", nil)

	e := NewContextEngine(ContextEngineConfig{
		Store:            store,
		Embedder:         angledEmbedder{query: []float32{1, 0}},
		MinSemanticScore: 0.25,
	})
	got := e.Assemble(operatorTurn(context.Background()), "when is the sourdough fed").Rendered()
	if !strings.Contains(got, "sourdough") {
		t.Errorf("a corpus with no vectors recalled nothing; the lexical fallback is not reachable:\n%s", got)
	}
}

func TestLexicalFloorWithholdsWeakOverlap(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	seedEpisodicOnly(t, store, "e1", "the sourdough starter is fed on tuesdays", nil)

	// Four scoring terms, one of which appears: 0.25, under the
	// shipped 0.30 floor.
	const weak = "sourdough helicopter submarine escalator"
	e := NewContextEngine(ContextEngineConfig{Store: store})
	if got := e.Assemble(operatorTurn(context.Background()), weak).Rendered(); strings.Contains(got, "sourdough") {
		t.Errorf("one term in four cleared the default lexical floor:\n%s", got)
	}

	// The same query with the gate off proves the record was reachable
	// and the floor is what withheld it, rather than the query having
	// failed to match anything.
	off := NewContextEngine(ContextEngineConfig{Store: store, MinLexicalScore: -1})
	if got := off.Assemble(operatorTurn(context.Background()), weak).Rendered(); !strings.Contains(got, "sourdough") {
		t.Errorf("with the floor disabled the record should still be found:\n%s", got)
	}
}

// Zero means "take the default" for both floors, matching MaxRecall
// and MaxRecallTokens. It cannot mean "disable", because 0 is a
// legitimate cosine an operator might type — so the off switch is a
// negative value, and this pins that distinction.
func TestZeroFloorTakesTheDefaultAndNegativeDisables(t *testing.T) {
	t.Parallel()
	e := NewContextEngine(ContextEngineConfig{})
	if e.minSemanticScore != DefaultMinSemanticScore {
		t.Errorf("zero MinSemanticScore = %v, want the default %v",
			e.minSemanticScore, DefaultMinSemanticScore)
	}
	if e.minLexicalScore != DefaultMinLexicalScore {
		t.Errorf("zero MinLexicalScore = %v, want the default %v",
			e.minLexicalScore, DefaultMinLexicalScore)
	}
	off := NewContextEngine(ContextEngineConfig{MinSemanticScore: -1, MinLexicalScore: -1})
	if off.minSemanticScore >= 0 || off.minLexicalScore >= 0 {
		t.Errorf("a negative floor was overwritten: semantic=%v lexical=%v",
			off.minSemanticScore, off.minLexicalScore)
	}
}
