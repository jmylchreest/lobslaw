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

func challengesAt(t *testing.T, s *memory.Store, at time.Time) *challengeSource {
	t.Helper()
	return &challengeSource{
		store:   s,
		now:     func() time.Time { return at },
		askedAt: map[string]time.Time{},
	}
}

func TestChallengesAskOncePerDreamCycle(t *testing.T) {
	t.Parallel()
	s := newNoticeStore(t)
	seedConflict(t, s, "user:john")
	morning := time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC)
	seedDreamRun(t, s, morning.Add(-7*time.Hour)) // last night's 02:00

	src := challengesAt(t, s, morning)
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

// The first thing the user says carries the question — no waiting for
// a particular hour, because somebody typing is somebody awake.
func TestChallengesSurfaceOnFirstEngagement(t *testing.T) {
	t.Parallel()
	s := newNoticeStore(t)
	seedConflict(t, s, "user:john")

	src := challengesAt(t, s, time.Date(2026, 8, 31, 3, 15, 0, 0, time.UTC))
	got, err := src.Notices(context.Background(), "user:john")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("notices = %+v; want the question", got)
	}
}

// Nothing is scheduled for somebody the agent is about to speak to
// anyway. Two messages about things that could have been said in one
// is the agent being a nuisance.
func TestNoChallengeScheduledWhenSomethingIsAlreadyDue(t *testing.T) {
	t.Parallel()
	s := newNoticeStore(t)
	seedConflict(t, s, "user:john")
	seedCommitment(t, s, "user:john", "pending", time.Now().Add(2*time.Hour))

	due, err := memory.HasPendingCommitment(s, "user:john", time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("a pending commitment inside the window was not seen")
	}
}

// A commitment that has already fired is not a conversation about to
// happen.
func TestFiredCommitmentsDoNotCountAsScheduled(t *testing.T) {
	t.Parallel()
	s := newNoticeStore(t)
	seedCommitment(t, s, "user:john", "done", time.Now().Add(2*time.Hour))

	due, err := memory.HasPendingCommitment(s, "user:john", time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if due {
		t.Error("a commitment that already fired was counted as scheduled")
	}
}

// Somebody else's reminder is not this person's conversation.
func TestPendingCommitmentsAreOwnerScoped(t *testing.T) {
	t.Parallel()
	s := newNoticeStore(t)
	seedCommitment(t, s, "user:alice", "pending", time.Now().Add(2*time.Hour))

	due, err := memory.HasPendingCommitment(s, "user:john", time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if due {
		t.Error("alice's reminder was read as john's")
	}
}

// The hour is a fact about the person: 09:00 where they are, not a
// fixed offset from a 02:00 dream.
func TestScheduledChallengeLandsInTheUsersMorning(t *testing.T) {
	t.Parallel()
	// 02:00 UTC, when dream runs.
	at := time.Date(2026, 8, 31, 2, 0, 0, 0, time.UTC)

	for _, tc := range []struct{ tz string }{{"Europe/London"}, {"America/Los_Angeles"}, {"Asia/Tokyo"}} {
		due := nextChallengeTime(at, tc.tz)
		loc, err := time.LoadLocation(tc.tz)
		if err != nil {
			t.Fatal(err)
		}
		if h := due.In(loc).Hour(); h != challengeHour {
			t.Errorf("%s: scheduled for %02d:00 local, want %02d:00", tc.tz, h, challengeHour)
		}
		if !due.After(at) {
			t.Errorf("%s: scheduled in the past (%s)", tc.tz, due)
		}
	}
}

func seedCommitment(t *testing.T, s *memory.Store, owner, status string, due time.Time) {
	t.Helper()
	c := &lobslawv1.AgentCommitment{
		Id: "commit-" + owner + status, Owner: owner, Status: status,
		DueAt: timestamppb.New(due), Trigger: "time",
	}
	raw, err := proto.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(memory.BucketCommitments, c.Id, raw); err != nil {
		t.Fatal(err)
	}
}
