package embedder

import (
	"sync"
	"testing"
)

// GOROUTINE SAFETY IS A CLAIM, SO IT GETS A TEST.
//
// A Model is read-only after Load — every weight is written once and
// never touched again, and hiddenStates allocates all of its scratch
// per call. That is easy to state and easy to break: a later
// optimisation that hoists the scratch onto the Model to save
// allocations would turn every concurrent Embed into a data race, and
// the symptom would be occasional subtly-wrong vectors rather than a
// crash.
//
// Run with -race, which is where this test earns its place.
func TestConcurrentEmbedIsSafeAndDeterministic(t *testing.T) {
	g := loadGolden(t)
	m, err := Load(modelDir(t))
	if err != nil {
		t.Fatal(err)
	}

	// Every goroutine embeds every fixture, and all must agree
	// BIT-EXACTLY with a single-threaded pass. Not "close": the
	// summation order inside one Embed does not depend on how many
	// other Embeds are running, so any difference at all is a race.
	want := make([][]float32, len(g.Fixtures))
	for i, f := range g.Fixtures {
		want[i] = m.Embed(f.TokenIDs)
	}

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan string, workers*len(g.Fixtures))
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i, f := range g.Fixtures {
				got := m.Embed(f.TokenIDs)
				if len(got) != len(want[i]) {
					errs <- "length mismatch"
					return
				}
				for j := range got {
					if got[j] != want[i][j] {
						errs <- "worker saw a different vector for " + f.Note
						return
					}
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// The same for the batch and chunked entry points, which allocate
// differently and could each grow their own shared state.
func TestConcurrentBatchAndLongAreSafe(t *testing.T) {
	g := loadGolden(t)
	m, err := Load(modelDir(t))
	if err != nil {
		t.Fatal(err)
	}
	seqs := make([][]int32, 0, len(g.Fixtures))
	for _, f := range g.Fixtures {
		seqs = append(seqs, f.TokenIDs)
	}
	long := make([]int32, 0, 4000)
	for range 4000 / 8 {
		long = append(long, 0, 70, 38937, 83, 35509, 23, 5753, 2)
	}

	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.EmbedBatch(seqs)
			_ = m.EmbedLong(long, 0, 2)
		}()
	}
	wg.Wait()
}
