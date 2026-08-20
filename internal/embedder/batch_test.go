package embedder

import (
	"math"
	"testing"
)

func TestEmbedBatchMatchesEmbedOneAtATime(t *testing.T) {
	g := loadGolden(t)
	m, err := Load(modelDir(t))
	if err != nil {
		t.Fatal(err)
	}
	seqs := make([][]int32, 0, len(g.Fixtures))
	for _, f := range g.Fixtures {
		seqs = append(seqs, f.TokenIDs)
	}
	got := m.EmbedBatch(seqs)
	if len(got) != len(seqs) {
		t.Fatalf("EmbedBatch returned %d vectors for %d inputs", len(got), len(seqs))
	}
	for i, f := range g.Fixtures {
		want := m.Embed(f.TokenIDs)
		for j := range want {
			// Bit-exact: batching must be a loop, not a different
			// computation. Any drift means padding or a shared buffer
			// crept in.
			if got[i][j] != want[j] {
				t.Fatalf("fixture %q element %d: batch %v, single %v", f.Note, j, got[i][j], want[j])
			}
		}
	}
}

// A sequence within the context limit must take the ordinary path, so
// chunking cannot perturb the common case.
func TestEmbedLongIsExactlyEmbedForShortInput(t *testing.T) {
	g := loadGolden(t)
	m, err := Load(modelDir(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range g.Fixtures {
		if len(f.TokenIDs) == 0 {
			continue
		}
		a := m.Embed(f.TokenIDs)
		b := m.EmbedLong(f.TokenIDs, 0, 2)
		for j := range a {
			if a[j] != b[j] {
				t.Fatalf("fixture %q: EmbedLong diverged from Embed on a short input", f.Note)
			}
		}
	}
}

// THE POINT OF CHUNKING. Embed truncates at the context limit, so a
// long input's tail is silently discarded — recall would never match
// anything said in the second half. EmbedLong must actually read it.
func TestEmbedLongSeesContentBeyondTheContextLimit(t *testing.T) {
	m, err := Load(modelDir(t))
	if err != nil {
		t.Fatal(err)
	}
	filler := func(n int) []int32 {
		out := make([]int32, 0, n)
		for len(out) < n {
			out = append(out, 70, 38937, 83, 35509, 23)
		}
		return out[:n]
	}
	// Identical for 700 tokens and different only afterwards. Sized to
	// clear a 512-token window by one chunk and no more: under -race on
	// a CI runner every extra token is real wall-clock, and a longer
	// head proves nothing this one does not.
	head := filler(700)
	a := append(append([]int32{0}, head...), append([]int32{5753, 72567}, 2)...)
	b := append(append([]int32{0}, head...), append([]int32{99999, 12345}, 2)...)

	if !vectorsDiffer(m.EmbedLong(a, 0, 2), m.EmbedLong(b, 0, 2)) {
		t.Error("EmbedLong gave identical vectors for inputs differing only past the context limit")
	}
	// And the truncating path must NOT see it — otherwise the test
	// above proves nothing about chunking.
	if vectorsDiffer(m.Embed(a), m.Embed(b)) {
		t.Error("Embed distinguished them, so this fixture does not exercise truncation")
	}
}

func vectorsDiffer(a, b []float32) bool {
	for i := range a {
		if math.Abs(float64(a[i]-b[i])) > 1e-6 {
			return true
		}
	}
	return false
}

// Length weighting: a short trailing chunk must not count as much as a
// full one. Built from the helpers so it needs no model.
func TestBoundaryHelpersRoundTrip(t *testing.T) {
	t.Parallel()
	ids := []int32{0, 10, 11, 12, 2}
	body := stripBoundaries(ids, 0, 2)
	if len(body) != 3 || body[0] != 10 || body[2] != 12 {
		t.Fatalf("stripBoundaries = %v, want [10 11 12]", body)
	}
	back := wrapBoundaries(body, 0, 2)
	for i := range ids {
		if back[i] != ids[i] {
			t.Fatalf("round trip = %v, want %v", back, ids)
		}
	}
	// Absent boundaries must be left alone rather than stripping real
	// tokens off the ends.
	plain := []int32{10, 11, 12}
	if got := stripBoundaries(plain, 0, 2); len(got) != 3 {
		t.Errorf("stripBoundaries removed real tokens: %v", got)
	}
	if got := wrapBoundaries(plain, -1, -1); len(got) != 3 {
		t.Errorf("wrapBoundaries added ids when told not to: %v", got)
	}
}
