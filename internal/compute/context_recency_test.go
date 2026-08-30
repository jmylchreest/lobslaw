package compute

import (
	"math"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// Recency has to change the order, or it is decoration.
//
// Ranking was pure similarity: a three-year-old fact and a
// three-minute-old one competed on wording alone, and the sort's
// timestamp tie-break never fired because cosine floats do not tie.
func TestRecencyBreaksATieSimilarityCannot(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	old := recencyWeight(timestamppb.New(now.Add(-2*365*24*time.Hour)), now)
	fresh := recencyWeight(timestamppb.New(now.Add(-time.Hour)), now)

	if !(fresh > old) {
		t.Errorf("a fresh record weighs %v and a two-year-old one %v; recency changes nothing", fresh, old)
	}
}

// Weighting must not become deletion.
//
// Without a floor, exp decay takes a year-old record to a rounding
// error — "the user lives in Leeds", said once and still true, stops
// being recallable at all. Recency should tilt the ordering, never
// overrule relevance outright.
func TestAnOldMemoryIsDemotedNotErased(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	for _, age := range []time.Duration{
		365 * 24 * time.Hour,
		5 * 365 * 24 * time.Hour,
		50 * 365 * 24 * time.Hour,
	} {
		got := recencyWeight(timestamppb.New(now.Add(-age)), now)
		if got < recallRecencyFloor {
			t.Errorf("a record aged %v weighs %v, below the floor %v", age, got, recallRecencyFloor)
		}
		// A highly relevant old record must still be able to outrank a
		// barely relevant fresh one.
		oldStrong := 0.9 * got
		freshWeak := 0.4 * recencyWeight(timestamppb.New(now), now)
		if oldStrong <= freshWeak {
			t.Errorf("aged %v: a strong old match (%v) lost to a weak fresh one (%v); "+
				"recency has overruled relevance", age, oldStrong, freshWeak)
		}
	}
}

// An undated record is not a stale one. Age unknown is not age proven,
// and penalising it would demote every record written before
// timestamps existed.
func TestAnUndatedRecordIsNotPenalised(t *testing.T) {
	t.Parallel()

	if got := recencyWeight(nil, time.Now()); got != 1 {
		t.Errorf("an undated record weighs %v, want 1", got)
	}
}

// Clock skew between nodes must not let a record score above a fresh
// one by claiming the future.
func TestAFutureTimestampDoesNotOutrankNow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	future := recencyWeight(timestamppb.New(now.Add(24*time.Hour)), now)
	present := recencyWeight(timestamppb.New(now), now)
	if future > present {
		t.Errorf("a record dated in the future weighs %v against %v for now", future, present)
	}
}

// The half-life means what it says: at exactly one, the record has
// given up half of what recency can cost it.
func TestTheHalfLifeIsTheHalfLife(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	got := recencyWeight(timestamppb.New(now.Add(-recallHalfLife)), now)
	want := recallRecencyFloor + (1-recallRecencyFloor)*0.5
	if math.Abs(float64(got)-want) > 0.001 {
		t.Errorf("at one half-life the weight is %v, want %v", got, want)
	}
}
