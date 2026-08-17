package memory

import (
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Rebinding rewrites ownership, so the test that matters most is not
// "did it move Alice's records" but "did it leave everybody else's
// alone". A migration that over-reaches is worse than one that does
// nothing: nothing is recoverable by re-running, over-reach is not.

func rebindTestStore(t *testing.T) *Store {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func putRaw(t *testing.T, s *Store, bucket, key string, m proto.Message) {
	t.Helper()
	raw, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(bucket, key, raw); err != nil {
		t.Fatal(err)
	}
}

// seedForRebind lays down one record per rewritten bucket owned by
// Alice's old id, plus a matching one owned by somebody else.
func seedForRebind(t *testing.T, s *Store) {
	t.Helper()
	const mine, theirs = "user:tg-@alice", "user:bob"

	putRaw(t, s, BucketVectorRecords, "v1", &lobslawv1.VectorRecord{Id: "v1", Owner: mine})
	putRaw(t, s, BucketVectorRecords, "v2", &lobslawv1.VectorRecord{Id: "v2", Owner: theirs})
	putRaw(t, s, BucketEpisodicRecords, "e1", &lobslawv1.EpisodicRecord{Id: "e1", Owner: mine})
	putRaw(t, s, BucketEpisodicRecords, "e2", &lobslawv1.EpisodicRecord{Id: "e2", Owner: theirs})
	putRaw(t, s, BucketCommitments, "c1", &lobslawv1.AgentCommitment{Id: "c1", Owner: mine})
	putRaw(t, s, BucketScheduledTasks, "t1", &lobslawv1.ScheduledTaskRecord{Id: "t1", Owner: mine})
	putRaw(t, s, BucketPrompts, "p1", &lobslawv1.PromptRecord{Id: "p1", Owner: mine})

	// Sessions key on the bare id, not the principal form.
	putRaw(t, s, BucketSessions, "s1", &lobslawv1.SessionRecord{Id: "s1", UserId: "tg-@alice"})
	putRaw(t, s, BucketSessions, "s2", &lobslawv1.SessionRecord{Id: "s2", UserId: "bob"})

	putRaw(t, s, BucketPolicyRules, "approval:p1", &lobslawv1.PolicyRule{
		Id: "approval:p1", Subject: mine, Action: "tool:exec",
		Resource: "write_file", Effect: "allow", CreatedBy: "approval:p1",
	})
	putRaw(t, s, BucketPolicyRules, "operator-rule", &lobslawv1.PolicyRule{
		Id: "operator-rule", Subject: "*", Action: "*", Resource: "*", Effect: "allow",
	})
	// A role subject names a group, not a person.
	putRaw(t, s, BucketPolicyRules, "role-rule", &lobslawv1.PolicyRule{
		Id: "role-rule", Subject: "role:operator", Action: "*", Resource: "*", Effect: "allow",
	})
}

func TestRebindMovesEverythingTheUserOwns(t *testing.T) {
	t.Parallel()
	s := rebindTestStore(t)
	seedForRebind(t, s)

	plan, err := PlanRebind(s, "tg-@alice", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Total() != 7 {
		t.Fatalf("plan covers %d records, want 7: %+v", plan.Total(), plan.Changes)
	}
	if err := ApplyRebindOffline(s, "tg-@alice", "alice"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		bucket, key string
		decode      func([]byte) string
	}{
		{BucketVectorRecords, "v1", func(b []byte) string {
			var r lobslawv1.VectorRecord
			_ = proto.Unmarshal(b, &r)
			return r.Owner
		}},
		{BucketEpisodicRecords, "e1", func(b []byte) string {
			var r lobslawv1.EpisodicRecord
			_ = proto.Unmarshal(b, &r)
			return r.Owner
		}},
		{BucketCommitments, "c1", func(b []byte) string {
			var r lobslawv1.AgentCommitment
			_ = proto.Unmarshal(b, &r)
			return r.Owner
		}},
		{BucketScheduledTasks, "t1", func(b []byte) string {
			var r lobslawv1.ScheduledTaskRecord
			_ = proto.Unmarshal(b, &r)
			return r.Owner
		}},
		{BucketPrompts, "p1", func(b []byte) string {
			var r lobslawv1.PromptRecord
			_ = proto.Unmarshal(b, &r)
			return r.Owner
		}},
		{BucketPolicyRules, "approval:p1", func(b []byte) string {
			var r lobslawv1.PolicyRule
			_ = proto.Unmarshal(b, &r)
			return r.Subject
		}},
	} {
		raw, err := s.Get(tc.bucket, tc.key)
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.bucket, tc.key, err)
		}
		if got := tc.decode(raw); got != "user:alice" {
			t.Errorf("%s/%s = %q, want user:alice", tc.bucket, tc.key, got)
		}
	}

	raw, err := s.Get(BucketSessions, "s1")
	if err != nil {
		t.Fatal(err)
	}
	var sess lobslawv1.SessionRecord
	_ = proto.Unmarshal(raw, &sess)
	if sess.UserId != "alice" {
		t.Errorf("session user_id = %q, want the bare id alice", sess.UserId)
	}
}

// The one that matters. Over-reach is not recoverable by re-running.
func TestRebindLeavesEverybodyElseAlone(t *testing.T) {
	t.Parallel()
	s := rebindTestStore(t)
	seedForRebind(t, s)

	if err := ApplyRebindOffline(s, "tg-@alice", "alice"); err != nil {
		t.Fatal(err)
	}

	var v lobslawv1.VectorRecord
	raw, _ := s.Get(BucketVectorRecords, "v2")
	_ = proto.Unmarshal(raw, &v)
	if v.Owner != "user:bob" {
		t.Errorf("bob's record owner = %q; the rebind took somebody else's data", v.Owner)
	}

	var sess lobslawv1.SessionRecord
	raw, _ = s.Get(BucketSessions, "s2")
	_ = proto.Unmarshal(raw, &sess)
	if sess.UserId != "bob" {
		t.Errorf("bob's session user_id = %q", sess.UserId)
	}

	for _, id := range []string{"operator-rule", "role-rule"} {
		var rule lobslawv1.PolicyRule
		raw, err := s.Get(BucketPolicyRules, id)
		if err != nil {
			t.Fatal(err)
		}
		_ = proto.Unmarshal(raw, &rule)
		if rule.Subject == "user:alice" {
			t.Errorf("%s was repointed at alice; its subject names a group, not a person", id)
		}
	}
}

// A dry run must write nothing. It is the default, so a plan that
// mutated would be a migration nobody asked to run.
func TestPlanRebindWritesNothing(t *testing.T) {
	t.Parallel()
	s := rebindTestStore(t)
	seedForRebind(t, s)

	if _, err := PlanRebind(s, "tg-@alice", "alice"); err != nil {
		t.Fatal(err)
	}

	raw, err := s.Get(BucketVectorRecords, "v1")
	if err != nil {
		t.Fatal(err)
	}
	var v lobslawv1.VectorRecord
	_ = proto.Unmarshal(raw, &v)
	if v.Owner != "user:tg-@alice" {
		t.Errorf("owner = %q; planning wrote to the store", v.Owner)
	}
}

// Both shapes have been written into owner fields over the project's
// life. Understanding only one would leave the other behind.
func TestRebindHandlesBareAndPrefixedOwners(t *testing.T) {
	t.Parallel()
	s := rebindTestStore(t)
	putRaw(t, s, BucketVectorRecords, "prefixed",
		&lobslawv1.VectorRecord{Id: "prefixed", Owner: "user:tg-@alice"})
	putRaw(t, s, BucketVectorRecords, "bare",
		&lobslawv1.VectorRecord{Id: "bare", Owner: "tg-@alice"})

	if err := ApplyRebindOffline(s, "tg-@alice", "alice"); err != nil {
		t.Fatal(err)
	}

	for key, want := range map[string]string{
		"prefixed": "user:alice",
		"bare":     "alice",
	} {
		raw, err := s.Get(BucketVectorRecords, key)
		if err != nil {
			t.Fatal(err)
		}
		var v lobslawv1.VectorRecord
		_ = proto.Unmarshal(raw, &v)
		if v.Owner != want {
			t.Errorf("%s owner = %q, want %q — the owner shape was not preserved", key, v.Owner, want)
		}
	}
}

// Prefs are keyed BY the id, so a rebind would have to merge two
// records. Reported rather than attempted: silently picking a winner
// between two timezones is worse than saying so.
func TestRebindReportsAPrefsConflictRatherThanMerging(t *testing.T) {
	t.Parallel()
	s := rebindTestStore(t)
	putRaw(t, s, BucketUserPrefs, "tg-@alice",
		&lobslawv1.UserPreferences{UserId: "tg-@alice", Timezone: "Europe/London"})

	plan, err := PlanRebind(s, "tg-@alice", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Conflicts) != 1 {
		t.Fatalf("conflicts = %v, want one about user_prefs", plan.Conflicts)
	}

	if err := ApplyRebindOffline(s, "tg-@alice", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(BucketUserPrefs, "tg-@alice"); err != nil {
		t.Error("the prefs record was moved or deleted despite being reported as a conflict")
	}
}

// Nothing to move is a clean no-op, not an error. An operator checking
// before they bind somebody should not be told off.
func TestRebindOnAnUnknownIDIsANoOp(t *testing.T) {
	t.Parallel()
	s := rebindTestStore(t)
	seedForRebind(t, s)

	plan, err := PlanRebind(s, "tg-@nobody", "nobody")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Total() != 0 {
		t.Errorf("plan covers %d records for an id that owns nothing: %+v", plan.Total(), plan.Changes)
	}
}
