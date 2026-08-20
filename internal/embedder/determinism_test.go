package embedder

import "testing"

// PARALLEL OVER HEADS MUST STILL BE DETERMINISTIC.
//
// Each head writes a disjoint slice of the scratch and reduces in a
// fixed order, so the result cannot depend on which head finishes
// first. If that ever stopped being true, embeddings would differ run
// to run by an amount too small to notice and too large to explain —
// so this asserts bit-exact equality across repeated calls, not
// closeness.
func TestAttentionIsBitExactAcrossRuns(t *testing.T) {
	g := loadGolden(t)
	m, err := Load(modelDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	for _, f := range g.Fixtures {
		if len(f.TokenIDs) == 0 {
			continue
		}
		want := m.Embed(f.TokenIDs)
		for range 12 {
			got := m.Embed(f.TokenIDs)
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("%s: element %d differs between runs (%v vs %v)", f.Note, i, got[i], want[i])
				}
			}
		}
	}
}
