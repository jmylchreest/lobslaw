package node

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// seedConflict writes a conflict verdict and the two memories it is
// about, which is what makes it a live question.
func seedConflict(t *testing.T, s *memory.Store, owner string) {
	t.Helper()
	for _, id := range []string{"a", "b"} {
		rec := &lobslawv1.EpisodicRecord{
			Id: id, Owner: owner, Context: "something " + id,
			Timestamp: timestamppb.New(time.Now()),
		}
		raw, err := proto.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Put(memory.BucketEpisodicRecords, id, raw); err != nil {
			t.Fatal(err)
		}
	}
	v := &lobslawv1.ConsolidationRecord{
		Id: "c1", ClusterId: "cl1", Verdict: string(memory.VerdictConflict),
		Reason:    "Are you vegetarian, or was the steak the exception?",
		SourceIds: []string{"a", "b"}, Owner: owner,
		CreatedAt: timestamppb.New(time.Now()),
	}
	raw, err := proto.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(memory.BucketConsolidations, v.Id, raw); err != nil {
		t.Fatal(err)
	}
}

func seedDreamRun(t *testing.T, s *memory.Store, at time.Time) {
	t.Helper()
	rec := &lobslawv1.EpisodicRecord{
		Id:        "dream-session-" + time.Duration(at.UnixNano()).String(),
		Event:     "dream run",
		Timestamp: timestamppb.New(at),
	}
	raw, err := proto.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(memory.BucketEpisodicRecords, rec.Id, raw); err != nil {
		t.Fatal(err)
	}
}

func nightmaresAt(t *testing.T, s *memory.Store, at time.Time) *nightmareSource {
	t.Helper()
	return &nightmareSource{
		store:   s,
		tz:      func(string) string { return "Europe/London" },
		now:     func() time.Time { return at },
		askedAt: map[string]time.Time{},
	}
}

// Dream runs at 02:00 and finds the contradiction then. Asking then
// would be a question at two in the morning about whether somebody is
// still vegetarian.
func TestNightmaresAreNotRaisedInTheSmallHours(t *testing.T) {
	t.Parallel()
	s := newNoticeStore(t)
	seedConflict(t, s, "user:john")

	src := nightmaresAt(t, s, time.Date(2026, 8, 31, 2, 30, 0, 0, time.UTC))
	got, err := src.Notices(context.Background(), "user:john")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("asked at 02:30: %+v", got)
	}
}

// The first conversation of the morning carries it.
func TestNightmaresSurfaceInTheMorning(t *testing.T) {
	t.Parallel()
	s := newNoticeStore(t)
	seedConflict(t, s, "user:john")

	src := nightmaresAt(t, s, time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC))
	got, err := src.Notices(context.Background(), "user:john")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("notices = %+v; want the question", got)
	}
}

// Asked once, not again until dream has looked at it afresh. A
// contradiction raised this morning and not yet answered has not
// earned a second mention before anything re-examined it.
func TestNightmaresAskOncePerDreamCycle(t *testing.T) {
	t.Parallel()
	s := newNoticeStore(t)
	seedConflict(t, s, "user:john")
	morning := time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC)
	seedDreamRun(t, s, morning.Add(-7*time.Hour)) // last night's 02:00

	src := nightmaresAt(t, s, morning)
	if got, _ := src.Notices(context.Background(), "user:john"); len(got) != 1 {
		t.Fatalf("first ask produced %+v", got)
	}

	// Later the same day: nothing has re-examined it.
	src.now = func() time.Time { return morning.Add(4 * time.Hour) }
	src.lastRunRead = time.Time{}
	if got, _ := src.Notices(context.Background(), "user:john"); len(got) != 0 {
		t.Errorf("asked again the same day: %+v", got)
	}

	// After another pass, the question has survived another night.
	next := morning.Add(24 * time.Hour)
	seedDreamRun(t, s, next.Add(-7*time.Hour))
	src.now = func() time.Time { return next }
	src.lastRunRead = time.Time{}
	if got, _ := src.Notices(context.Background(), "user:john"); len(got) != 1 {
		t.Errorf("a contradiction that survived another night was not raised again: %+v", got)
	}
}

// Morning is a fact about the person, not the machine.
func TestNightmaresFollowTheUsersTimezone(t *testing.T) {
	t.Parallel()
	s := newNoticeStore(t)
	seedConflict(t, s, "user:john")

	// 15:00 UTC is 08:00 in Los Angeles and 02:00 the next day in Tokyo.
	at := time.Date(2026, 8, 31, 15, 0, 0, 0, time.UTC)

	la := nightmaresAt(t, s, at)
	la.tz = func(string) string { return "America/Los_Angeles" }
	if got, _ := la.Notices(context.Background(), "user:john"); len(got) != 1 {
		t.Errorf("nothing asked at 08:00 local in Los Angeles: %+v", got)
	}

	tokyo := nightmaresAt(t, s, at)
	tokyo.tz = func(string) string { return "Asia/Tokyo" }
	if got, _ := tokyo.Notices(context.Background(), "user:john"); len(got) != 0 {
		t.Errorf("asked at midnight in Tokyo: %+v", got)
	}
}

func newNoticeStore(t *testing.T) *memory.Store {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(t.TempDir(), "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
