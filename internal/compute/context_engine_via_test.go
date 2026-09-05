package compute

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// PROVENANCE IN THE RECALL BLOCK.
//
// The incident this closes: asked "Hey you there?", the assistant
// answered "Galaxy chocolate is still on the list from the other day,
// nothing has changed since" — a claim about a shared list that three
// other people had edited since, stated as present fact from a memory
// of its own past action.
//
// when= was rendered on that block and made no difference, which is
// the point. A date does not say which memories expire. "Prefers
// terse replies" and "added chocolate to the list" are both true
// accounts of the past; only the second stops being true when
// somebody else opens the app. Nothing in the prose separates them —
// but one of them was produced by calling another system, and that is
// recorded.

func TestRecallRendersToolProvenance(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	seedEpisodic(t, store, &lobslawv1.EpisodicRecord{
		Id: "e1", Event: "added chocolate", Context: "added galaxy chocolate to the shopping list",
		Importance: 5, Timestamp: timestamppb.Now(),
		Owner: "user:alice", Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE,
		Via: []string{"kitchenowl_add_shoppinglist_item"},
	})
	seedVector(t, store, &lobslawv1.VectorRecord{
		Id: "vec-e1", Embedding: []float32{1, 0}, SourceIds: []string{"e1"},
		Owner: "user:alice", Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE,
	})

	e := NewContextEngine(ContextEngineConfig{
		Store: store, Embedder: angledEmbedder{query: []float32{1, 0}},
	})
	got := e.Assemble(operatorTurn(context.Background()), "is the chocolate on the list").Rendered()

	if !strings.Contains(got, "via=kitchenowl_add_shoppinglist_item") {
		t.Errorf("recall block does not carry the tool provenance:\n%s", got)
	}
	// The existing attributes are not displaced by the new one.
	if !strings.Contains(got, "when=") || !strings.Contains(got, "score=") {
		t.Errorf("via= displaced an existing attribute:\n%s", got)
	}
}

// A memory the model produced from the conversation alone carries no
// via=, and must not gain an empty one — "via=" with nothing after it
// reads as a tool whose name was lost, which is a worse claim than
// silence.
func TestRecallOmitsProvenanceWhenNoToolRan(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	seedRecallable(t, store, "e1", "john prefers terse replies without preamble", nil)

	e := NewContextEngine(ContextEngineConfig{
		Store: store, Embedder: angledEmbedder{query: []float32{1, 0}},
	})
	got := e.Assemble(operatorTurn(context.Background()), "how should I write replies").Rendered()

	if !strings.Contains(got, "terse") {
		t.Fatalf("the record was not recalled at all:\n%s", got)
	}
	if strings.Contains(got, "via=") {
		t.Errorf("a memory with no tool calls rendered a via= attribute:\n%s", got)
	}
}

// A turn that called many tools must not have its metadata outgrow the
// memory it describes.
func TestRecallBoundsRenderedProvenance(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	seedEpisodic(t, store, &lobslawv1.EpisodicRecord{
		Id: "e1", Event: "did a lot", Context: "ran a long errand across several systems",
		Importance: 5, Timestamp: timestamppb.Now(),
		Owner: "user:alice", Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE,
		Via: []string{"t_alpha", "t_bravo", "t_charlie", "t_delta", "t_echo"},
	})
	seedVector(t, store, &lobslawv1.VectorRecord{
		Id: "vec-e1", Embedding: []float32{1, 0}, SourceIds: []string{"e1"},
		Owner: "user:alice", Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE,
	})

	e := NewContextEngine(ContextEngineConfig{
		Store: store, Embedder: angledEmbedder{query: []float32{1, 0}},
	})
	got := e.Assemble(operatorTurn(context.Background()), "what did you do").Rendered()

	if strings.Contains(got, "t_delta") || strings.Contains(got, "t_echo") {
		t.Errorf("more than %d tool names reached the prompt:\n%s", maxRenderedVia, got)
	}
	if !strings.Contains(got, "t_alpha") {
		t.Errorf("the bound dropped everything instead of trimming:\n%s", got)
	}
}

func TestInvokedToolNamesIsSortedAndDeduplicated(t *testing.T) {
	t.Parallel()
	got := invokedToolNames([]ToolInvocation{
		{ToolName: "shell_run"},
		{ToolName: "kitchenowl_list"},
		{ToolName: "shell_run"},
		{ToolName: ""},
		{ToolName: "kitchenowl_list"},
	})
	want := []string{"kitchenowl_list", "shell_run"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	// A turn that called nothing yields nil rather than an empty
	// slice, so the proto field stays absent instead of present-and-
	// empty.
	if invokedToolNames(nil) != nil {
		t.Error("a turn with no tool calls should produce nil")
	}
}
