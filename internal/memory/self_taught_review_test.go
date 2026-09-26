package memory

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestReviewedSkillDecision(t *testing.T) {
	t.Parallel()
	for _, approve := range []bool{true, false} {
		t.Run(map[bool]string{true: "approve", false: "deny"}[approve], func(t *testing.T) {
			t.Parallel()
			s := selfTaught(t, SelfLearningPropose)
			_, err := s.Propose(t.Context(), aSkill("review-me"), ProposeIntent{})
			if err != nil {
				t.Fatal(err)
			}
			rec, err := s.Get("skill:review-me")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.DecideReviewed(t.Context(), rec.Id, rec.Revision, SelfTaughtReviewDigest(rec), "user:bob", approve); err == nil {
				t.Fatal("cross-owner decision accepted")
			}
			if _, err = s.DecideReviewed(t.Context(), rec.Id, 0, SelfTaughtReviewDigest(rec), rec.Owner, approve); err == nil {
				t.Fatal("unversioned decision accepted")
			}
			out, err := s.DecideReviewed(t.Context(), rec.Id, rec.Revision, SelfTaughtReviewDigest(rec), rec.Owner, approve)
			if err != nil {
				t.Fatal(err)
			}
			if approve && (out.State != lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_ACTIVE || out.ApprovedBy != rec.Owner) {
				t.Fatalf("bad approval: %v", out)
			}
			if !approve {
				rows, err := s.List(SelfTaughtQuery{Archived: true})
				if err != nil || len(rows) != 1 {
					t.Fatalf("archive: %v %v", rows, err)
				}
				if _, err = s.Get(rec.Id); err == nil {
					t.Fatal("denied proposal still live")
				}
			}
			if _, err = s.DecideReviewed(t.Context(), rec.Id, rec.Revision, SelfTaughtReviewDigest(rec), rec.Owner, approve); err == nil {
				t.Fatal("replayed decision accepted")
			}
		})
	}
}

func TestReviewedSkillRejectsChangedContent(t *testing.T) {
	t.Parallel()
	s := selfTaught(t, SelfLearningPropose)
	_, err := s.Propose(t.Context(), aSkill("review-me"), ProposeIntent{})
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Get("skill:review-me")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Propose(t.Context(), named("review-me", "changed", "new instructions"), ProposeIntent{Refines: before.Id, Rationale: "correction"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DecideReviewed(t.Context(), before.Id, before.Revision, SelfTaughtReviewDigest(before), before.Owner, true); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("stale decision: %v", err)
	}
	current, err := s.Get(before.Id)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.DecideReviewed(t.Context(), current.Id, current.Revision, SelfTaughtReviewDigest(current), current.Owner, true)
	if err != nil {
		t.Fatal(err)
	}
	if out.Body != "new instructions" || out.Pending != nil {
		t.Fatalf("did not approve reviewed refinement: %v", out)
	}
}

func TestReviewedRefinementDenialPreservesActiveSkill(t *testing.T) {
	t.Parallel()
	s := selfTaught(t, SelfLearningPropose)
	_, err := s.Propose(t.Context(), aSkill("review-me"), ProposeIntent{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Approve(t.Context(), "skill:review-me", "user:alice"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Propose(t.Context(), named("review-me", "changed", "new instructions"), ProposeIntent{Refines: "skill:review-me", Rationale: "correction"})
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.Get("skill:review-me")
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.DecideReviewed(t.Context(), current.Id, current.Revision, SelfTaughtReviewDigest(current), current.Owner, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Body != "how to do the thing" || out.Pending != nil || out.State != lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_ACTIVE {
		t.Fatalf("denial changed active skill: %v", out)
	}
}

func TestReviewedDecisionHasOneWinner(t *testing.T) {
	t.Parallel()
	s := selfTaught(t, SelfLearningPropose)
	if _, err := s.Propose(t.Context(), aSkill("contended"), ProposeIntent{}); err != nil {
		t.Fatal(err)
	}
	rec, err := s.Get("skill:contended")
	if err != nil {
		t.Fatal(err)
	}
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			if _, err := s.DecideReviewed(t.Context(), rec.Id, rec.Revision, SelfTaughtReviewDigest(rec), rec.Owner, i%2 == 0); err == nil {
				won.Add(1)
			}
		})
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("%d decisions succeeded", won.Load())
	}
}

func TestReviewedDigestRejectsReusedRevision(t *testing.T) {
	t.Parallel()
	s := selfTaught(t, SelfLearningPropose)
	if _, err := s.Propose(t.Context(), aSkill("restored"), ProposeIntent{}); err != nil {
		t.Fatal(err)
	}
	rec, err := s.Get("skill:restored")
	if err != nil {
		t.Fatal(err)
	}
	digest := SelfTaughtReviewDigest(rec)
	rec.Files = map[string]string{"references/changed.md": "unreviewed content"}
	raw, err := proto.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.Put(BucketSelfTaught, rec.Id, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideReviewed(t.Context(), rec.Id, rec.Revision, digest, rec.Owner, true); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("reused revision approved changed content: %v", err)
	}
}
