package ids

import (
	"sort"
	"sync"
	"testing"
)

// The bug this package exists to prevent: concurrent minting from a
// shared unguarded MonotonicReader can emit duplicate ULIDs, and
// records are keyed by ID in bbolt — so a collision silently
// overwrites another subsystem's record.
func TestNewIsUniqueUnderConcurrency(t *testing.T) {
	t.Parallel()
	const goroutines, perGoroutine = 16, 500

	var mu sync.Mutex
	seen := make(map[string]struct{}, goroutines*perGoroutine)
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			local := make([]string, 0, perGoroutine)
			for range perGoroutine {
				local = append(local, New())
			}
			mu.Lock()
			defer mu.Unlock()
			for _, id := range local {
				if _, dup := seen[id]; dup {
					t.Errorf("duplicate ULID %q", id)
					return
				}
				seen[id] = struct{}{}
			}
		})
	}
	wg.Wait()

	if len(seen) != goroutines*perGoroutine {
		t.Errorf("got %d unique ids, want %d", len(seen), goroutines*perGoroutine)
	}
}

// IDs minted in the same millisecond must still sort in creation
// order — that's the whole reason a shared monotonic source is used
// instead of a fresh one per call.
//
// Runs alongside the concurrency test on purpose (both t.Parallel):
// contention from another minting goroutine is exactly what broke
// ordering when the lock covered only the entropy read and not the
// clock read.
func TestNewIsMonotonicWithinAMillisecond(t *testing.T) {
	t.Parallel()
	const n = 1000
	got := make([]string, 0, n)
	for range n {
		got = append(got, New())
	}
	if !sort.StringsAreSorted(got) {
		for i := 1; i < len(got); i++ {
			if got[i-1] > got[i] {
				t.Fatalf("ids not monotonic at %d: %q > %q", i, got[i-1], got[i])
			}
		}
	}
}

func TestNewHasULIDShape(t *testing.T) {
	t.Parallel()
	id := New()
	if len(id) != 26 {
		t.Errorf("got %d chars (%q), want 26", len(id), id)
	}
}
