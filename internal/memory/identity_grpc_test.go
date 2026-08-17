package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// `lobslaw identity rebind` wrote straight to bbolt, so it needed the
// node stopped — and pointed at a follower's file while the cluster
// ran it would have written ownership no other replica has.
//
// Rebinding rewrites ownership, so the test that matters most is not
// "did it move Alice's records" but "did it leave everybody else's
// alone". A migration that over-reaches is worse than one that does
// nothing: nothing is recoverable by re-running, over-reach is not.

func newIdentityRPC(t *testing.T) (*IdentityRPC, *Service) {
	t.Helper()
	svc := newTestServiceStack(t)
	return NewIdentityRPC(svc.raft, svc.store), svc
}

func rebind(t *testing.T, rpc *IdentityRPC, req *lobslawv1.RebindRequest) *lobslawv1.RebindResponse {
	t.Helper()
	res, err := rpc.Rebind(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func vectorOwner(t *testing.T, s *Service, id string) string {
	t.Helper()
	raw, err := s.store.Get(BucketVectorRecords, id)
	if err != nil {
		t.Fatal(err)
	}
	var rec lobslawv1.VectorRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	return rec.GetOwner()
}

// THE POINT. The rewrite goes through raft, so every replica sees it —
// the offline path wrote to one file and required an outage to do it.
func TestARebindReplicates(t *testing.T) {
	t.Parallel()
	rpc, svc := newIdentityRPC(t)
	putVector(t, svc, "v1", "user:tg-@old", "")

	res := rebind(t, rpc, &lobslawv1.RebindRequest{From: "tg-@old", To: "tg-@new"})
	if res.GetApplied() != 1 {
		t.Fatalf("applied = %d, want 1", res.GetApplied())
	}
	if got := vectorOwner(t, svc, "v1"); got != "user:tg-@new" {
		t.Errorf("owner = %q, want user:tg-@new", got)
	}
}

// A dry run must WRITE NOTHING while still reporting what it would do.
// This rewrites ownership across seven buckets and there is no undo.
func TestARebindDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	rpc, svc := newIdentityRPC(t)
	putVector(t, svc, "v1", "user:tg-@old", "")

	res := rebind(t, rpc, &lobslawv1.RebindRequest{
		From: "tg-@old", To: "tg-@new", DryRun: true,
	})
	if res.GetApplied() != 0 {
		t.Errorf("applied = %d on a dry run", res.GetApplied())
	}
	if len(res.GetChanges()) != 1 || len(res.GetChanges()[0].GetIds()) != 1 {
		t.Errorf("changes = %v; a dry run should still say what it would move", res.GetChanges())
	}
	if got := vectorOwner(t, svc, "v1"); got != "user:tg-@old" {
		t.Fatalf("a dry run moved the record: owner = %q", got)
	}
}

// The one that matters most.
func TestARebindLeavesEverybodyElseAlone(t *testing.T) {
	t.Parallel()
	rpc, svc := newIdentityRPC(t)
	putVector(t, svc, "mine", "user:tg-@old", "")
	putVector(t, svc, "theirs", "user:tg-@someone", "")

	rebind(t, rpc, &lobslawv1.RebindRequest{From: "tg-@old", To: "tg-@new"})

	if got := vectorOwner(t, svc, "theirs"); got != "user:tg-@someone" {
		t.Errorf("somebody else's record moved: owner = %q", got)
	}
}

// Re-running must be safe. A rebind interrupted midway leaves some
// records moved and some not, and re-running is the recovery — so a
// record already owned by `to` must no longer match `from`.
func TestARebindIsIdempotent(t *testing.T) {
	t.Parallel()
	rpc, svc := newIdentityRPC(t)
	putVector(t, svc, "v1", "user:tg-@old", "")

	rebind(t, rpc, &lobslawv1.RebindRequest{From: "tg-@old", To: "tg-@new"})
	second := rebind(t, rpc, &lobslawv1.RebindRequest{From: "tg-@old", To: "tg-@new"})

	if second.GetApplied() != 0 {
		t.Errorf("the second run moved %d record(s); re-running is meant to be a no-op",
			second.GetApplied())
	}
	if got := vectorOwner(t, svc, "v1"); got != "user:tg-@new" {
		t.Errorf("owner = %q after two runs", got)
	}
}

// Somebody who typed the same id twice meant something else, and
// running it would report a confident zero.
func TestRebindingAnIdToItselfIsRefused(t *testing.T) {
	t.Parallel()
	rpc, _ := newIdentityRPC(t)

	_, err := rpc.Rebind(context.Background(),
		&lobslawv1.RebindRequest{From: "tg-@a", To: "tg-@a"})
	if err == nil {
		t.Fatal("rebinding an id to itself was accepted")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestRebindNeedsBothIds(t *testing.T) {
	t.Parallel()
	rpc, _ := newIdentityRPC(t)

	for name, req := range map[string]*lobslawv1.RebindRequest{
		"no from": {To: "tg-@new"},
		"no to":   {From: "tg-@old"},
		"neither": {},
	} {
		if _, err := rpc.Rebind(context.Background(), req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// An unknown id must report nothing rather than error: "nothing owned
// by that id" is a useful answer, and it is how somebody checks a
// rebind already happened.
func TestRebindingAnUnknownIdIsANoOp(t *testing.T) {
	t.Parallel()
	rpc, svc := newIdentityRPC(t)
	putVector(t, svc, "v1", "user:tg-@someone", "")

	res := rebind(t, rpc, &lobslawv1.RebindRequest{From: "tg-@ghost", To: "tg-@new"})
	if res.GetApplied() != 0 || len(res.GetChanges()) != 0 {
		t.Errorf("an unknown id moved something: %+v", res)
	}
	if got := vectorOwner(t, svc, "v1"); got != "user:tg-@someone" {
		t.Errorf("owner = %q", got)
	}
}

// Preferences are keyed BY the id, so a rebind would have to merge two
// records. Silently picking a winner between two timezones is worse
// than saying so.
func TestAPrefsConflictIsReportedNotMerged(t *testing.T) {
	t.Parallel()
	rpc, svc := newIdentityRPC(t)
	raw, err := proto.Marshal(&lobslawv1.UserPreferences{UserId: "tg-@old"})
	if err != nil {
		t.Fatal(err)
	}
	if perr := svc.store.Put(BucketUserPrefs, "tg-@old", raw); perr != nil {
		t.Fatal(perr)
	}

	res := rebind(t, rpc, &lobslawv1.RebindRequest{
		From: "tg-@old", To: "tg-@new", DryRun: true,
	})
	if len(res.GetConflicts()) == 0 {
		t.Fatal("the prefs record was not reported as a conflict")
	}
	// And it is still there — a conflict that was reported and then
	// merged anyway would be the worst of both.
	if _, gerr := svc.store.Get(BucketUserPrefs, "tg-@old"); gerr != nil {
		t.Error("the prefs record was moved despite being reported as a conflict")
	}
}

// The store is what a rebind rewrites; without one there is nothing to
// answer about, and answering "nothing moved" would read as success.
func TestAnUnwiredIdentityServiceRefuses(t *testing.T) {
	t.Parallel()
	rpc := NewIdentityRPC(nil, nil)
	if _, err := rpc.Rebind(context.Background(),
		&lobslawv1.RebindRequest{From: "a", To: "b"}); err == nil {
		t.Error("an unwired service reported a successful rebind")
	}
}

// --- the failure paths -------------------------------------------------

// scriptedApplier answers raft.Apply from a script.
type scriptedApplier struct {
	err      error
	fsmErr   error
	attempts int
}

func (f *scriptedApplier) Apply(_ []byte, _ time.Duration) (any, error) {
	f.attempts++
	if f.err != nil {
		return nil, f.err
	}
	if f.fsmErr != nil {
		return f.fsmErr, nil
	}
	return nil, nil
}

// An FSM that rejects the entry returns its error IN THE RESULT rather
// than from Apply. Ignoring it would report a rebind that never landed
// as a completed one — the worst outcome available here.
func TestAnFSMRejectionIsNotReportedAsSuccess(t *testing.T) {
	t.Parallel()
	_, svc := newIdentityRPC(t)
	putVector(t, svc, "v1", "user:tg-@old", "")

	applier := &scriptedApplier{fsmErr: errors.New("bucket refused the write")}
	applied, err := ApplyRebindReplicated(context.Background(), applier, svc.store, "tg-@old", "tg-@new")
	if err == nil {
		t.Fatal("an FSM rejection was reported as a successful rebind")
	}
	if applied != 0 {
		t.Errorf("applied = %d; nothing landed", applied)
	}
}

// A transport failure must stop, and say how far it got. "Rebind
// failed" without a number leaves somebody unable to tell a no-op from
// a half-done move.
func TestATransportFailureReportsHowFarItGot(t *testing.T) {
	t.Parallel()
	_, svc := newIdentityRPC(t)
	putVector(t, svc, "v1", "user:tg-@old", "")
	putVector(t, svc, "v2", "user:tg-@old", "")

	applier := &scriptedApplier{err: errors.New("no leader")}
	applied, err := ApplyRebindReplicated(context.Background(), applier, svc.store, "tg-@old", "tg-@new")
	if err == nil {
		t.Fatal("a transport failure was reported as success")
	}
	if applied != 0 {
		t.Errorf("applied = %d, want 0 — it failed on the first record", applied)
	}
	if applier.attempts != 1 {
		t.Errorf("attempts = %d; it kept going after a failure", applier.attempts)
	}
}

// And the successful path really does apply one entry per record.
func TestEveryMatchedRecordIsReplicatedOnce(t *testing.T) {
	t.Parallel()
	_, svc := newIdentityRPC(t)
	putVector(t, svc, "v1", "user:tg-@old", "")
	putVector(t, svc, "v2", "user:tg-@old", "")
	putVector(t, svc, "other", "user:tg-@someone", "")

	applier := &scriptedApplier{}
	applied, err := ApplyRebindReplicated(context.Background(), applier, svc.store, "tg-@old", "tg-@new")
	if err != nil {
		t.Fatal(err)
	}
	if applied != 2 || applier.attempts != 2 {
		t.Errorf("applied=%d attempts=%d, want 2 and 2", applied, applier.attempts)
	}
}

func TestReplicatingWithNoRaftIsRefused(t *testing.T) {
	t.Parallel()
	_, svc := newIdentityRPC(t)
	if _, err := ApplyRebindReplicated(context.Background(), nil, svc.store, "a", "b"); err == nil {
		t.Error("a rebind with no raft reported success")
	}
}
