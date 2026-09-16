package node

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// clampToExpiry can land DueAt exactly on ExpiresAt. That instant must
// expire, not probe and then re-arm into a full backoff past the end.
func TestWatchExpiryAtExactBoundary(t *testing.T) {
	t.Parallel()
	now := time.Now()
	st := newWatchState(time.Hour, 24*time.Hour)
	st.ExpiresAt = timestamppb.New(now)
	st.LastChanged = timestamppb.New(now.Add(-time.Hour))

	d, expired := decideWatchExpiry(st, "the fare", now)
	if !expired {
		t.Fatal("now == ExpiresAt must expire; After alone would still probe")
	}
	if d.Action != watchExpired {
		t.Fatalf("action = %s, want expired", d.Action)
	}
	if d.Retry != 0 {
		t.Errorf("boundary expiry must not re-arm; got %s", d.Retry)
	}
	if d.Message == "" {
		t.Error("boundary expiry must speak")
	}
}

func TestWatchClampAtOrPastExpiryDoesNotRearm(t *testing.T) {
	t.Parallel()
	now := time.Now()
	st := newWatchState(time.Hour, 24*time.Hour)
	st.ExpiresAt = timestamppb.New(now)
	st.Observation = "price: 212.00 GBP"
	st.Digest = watchDigest(st.Observation)
	st.UnchangedRuns = 20

	d := decideWatchResult(st, "the fare", report("price: 212.00 GBP", ""), now)
	if d.Action != watchUnchanged {
		t.Fatalf("action = %s, want unchanged", d.Action)
	}
	if d.Retry != 0 {
		t.Errorf("at expiry, clamp must return 0 not full backoff; got %s", d.Retry)
	}

	past := newWatchState(time.Hour, 24*time.Hour)
	past.ExpiresAt = timestamppb.New(now.Add(-time.Minute))
	if got := clampToExpiry(past, now, 24*time.Hour); got != 0 {
		t.Errorf("past expiry clamp = %s, want 0", got)
	}
}
