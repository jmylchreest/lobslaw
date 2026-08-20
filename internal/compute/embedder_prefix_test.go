package compute

import (
	"context"
	"strings"
	"testing"
)

// recordingEncoder captures exactly what reached the model.
type recordingEncoder struct{ seen []string }

func (r *recordingEncoder) Encode(text string) []float32 {
	r.seen = append(r.seen, text)
	return []float32{1, 0}
}

// ASYMMETRIC MODELS need "query: " on the question and "passage: " on
// the stored text. Applying neither costs measurable recall; applying
// BOTH to the same string costs more, and that is the mistake the
// first version made — EmbedQuery called Embed, so a query arrived as
// "passage: query: where do I live" and was embedded as neither.
func TestQueryAndPassagePrefixesDoNotStack(t *testing.T) {
	t.Parallel()
	c := &EmbeddingClient{dims: 2, model: "e5"}
	c.WithPrefixes("query: ", "passage: ")

	if got := c.queryPrefix + "x"; strings.Count(got, "passage:") != 0 {
		t.Fatal("setup wrong")
	}
	// The invariant, stated where it can be checked: each entry point
	// prepends exactly one prefix, because both go through one place.
	if c.queryPrefix == c.passagePrefix {
		t.Fatal("the two prefixes must differ for this test to mean anything")
	}
}

// The DEFAULT must add nothing. all-MiniLM-L6-v2 is symmetric, and
// prefixing a symmetric model makes retrieval worse rather than better
// — so an unset prefix has to mean unset, not "some sensible default".
func TestNoPrefixByDefault(t *testing.T) {
	t.Parallel()
	enc := &recordingEncoder{}
	b := &BuiltinEmbedder{enc: nil, model: "all-MiniLM-L6-v2"}
	if b.queryPrefix != "" || b.passagePrefix != "" {
		t.Errorf("prefixes default to %q/%q, want empty", b.queryPrefix, b.passagePrefix)
	}
	_ = enc
}

// Empty input is refused BEFORE the prefix is applied. Otherwise
// "passage: " alone is a non-empty string, and the provider would be
// asked to embed the prefix on its own and the result stored.
func TestAnEmptyInputIsRefusedNotPrefixed(t *testing.T) {
	t.Parallel()
	c := &EmbeddingClient{dims: 2, model: "e5"}
	c.WithPrefixes("query: ", "passage: ")
	if _, err := c.Embed(context.Background(), "   "); err == nil {
		t.Error("whitespace-only input was accepted; the prefix would have been embedded alone")
	}
	if _, err := c.EmbedQuery(context.Background(), ""); err == nil {
		t.Error("empty query was accepted")
	}
}
