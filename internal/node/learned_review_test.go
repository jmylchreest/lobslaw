package node

import (
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/skills"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type reviewApplier struct{ fsm *memory.FSM }

func (a reviewApplier) Apply(raw []byte, _ time.Duration) (any, error) {
	return a.fsm.Apply(&raft.Log{Data: raw}), nil
}

func reviewNode(t *testing.T) *Node {
	t.Helper()
	store := crossOwnerTestStore(t)
	st, err := memory.NewSelfTaughtStore(reviewApplier{memory.NewFSM(store)}, store, memory.SelfLearningPropose)
	if err != nil {
		t.Fatal(err)
	}
	mat, err := skills.NewMaterialiser(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	n := &Node{selfTaught: st, policyEngine: policy.NewEngine(store, slog.Default()), log: slog.Default(), materialiser: mat, skillRegistry: skills.NewRegistry(slog.Default())}
	seedRule(t, store, &lobslawv1.PolicyRule{Id: "learned-alice", Subject: "user:alice", Action: "command:exec", Resource: "learned", Effect: "allow", Priority: 50})
	return n
}

func TestLearnedReviewsScopeAndActivation(t *testing.T) {
	t.Parallel()
	n := reviewNode(t)
	for _, owner := range []string{"user:alice", "user:bob", ""} {
		name := "mine"
		if owner == "user:bob" {
			name = "theirs"
		}
		if owner == "" {
			name = "unowned"
		}
		_, err := n.selfTaught.Propose(t.Context(), &lobslawv1.SelfTaughtRecord{Kind: lobslawv1.SelfTaughtKind_SELF_TAUGHT_KIND_SKILL, Name: name, Body: "instructions for " + name, Owner: owner, Files: map[string]string{"references/note.md": "reference"}}, memory.ProposeIntent{})
		if err != nil {
			t.Fatal(err)
		}
	}
	notices, err := (pendingReviewSource{store: n.selfTaught}).Notices(t.Context(), "user:alice")
	if err != nil || len(notices) != 1 || !strings.Contains(notices[0].Text, "1 skill waiting") {
		t.Fatalf("notice disagrees with owner queue: %v %v", notices, err)
	}
	r := n.learnedReviews()
	alice := &types.Claims{UserID: "alice"}
	rows, err := r.List(t.Context(), alice)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list: %v %v", rows, err)
	}
	if _, err := r.Get(t.Context(), alice, "skill:theirs"); err == nil {
		t.Fatal("cross-owner read")
	}
	if _, err := r.Get(t.Context(), alice, "skill:unowned"); err == nil {
		t.Fatal("unowned read")
	}
	if _, err := r.List(t.Context(), &types.Claims{UserID: "bob"}); err == nil {
		t.Fatal("ungranted read")
	}
	out, err := r.Decide(t.Context(), alice, rows[0].ID, rows[0].Revision, rows[0].Digest, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "installed and active") {
		t.Fatalf("activation: %s", out)
	}
	if _, err := n.skillRegistry.Get("mine"); err != nil {
		t.Fatal(err)
	}
}

func TestLearnedReviewsDoNotClaimActivationWithoutMaterialiser(t *testing.T) {
	t.Parallel()
	n := reviewNode(t)
	n.materialiser = nil
	_, err := n.selfTaught.Propose(t.Context(), &lobslawv1.SelfTaughtRecord{Kind: lobslawv1.SelfTaughtKind_SELF_TAUGHT_KIND_SKILL, Name: "mine", Body: "instructions", Owner: "user:alice"}, memory.ProposeIntent{})
	if err != nil {
		t.Fatal(err)
	}
	r := n.learnedReviews()
	claims := &types.Claims{UserID: "alice"}
	rec, err := r.Get(t.Context(), claims, "skill:mine")
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Decide(t.Context(), claims, rec.ID, rec.Revision, rec.Digest, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pending") {
		t.Fatalf("claimed activation: %s", out)
	}
}

func TestLearnedReviewsReportActivationFailure(t *testing.T) {
	t.Parallel()
	n := reviewNode(t)
	_, err := n.selfTaught.Propose(t.Context(), &lobslawv1.SelfTaughtRecord{Kind: lobslawv1.SelfTaughtKind_SELF_TAUGHT_KIND_SKILL, Name: "mine", Body: "instructions", Owner: "user:alice"}, memory.ProposeIntent{})
	if err != nil {
		t.Fatal(err)
	}
	// A filesystem error after approval must not be reported as activation.
	if err := os.RemoveAll(n.materialiser.Root()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n.materialiser.Root(), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	r := n.learnedReviews()
	claims := &types.Claims{UserID: "alice"}
	rec, err := r.Get(t.Context(), claims, "skill:mine")
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Decide(t.Context(), claims, rec.ID, rec.Revision, rec.Digest, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Approval recorded") || (!strings.Contains(out, "failed") && !strings.Contains(out, "not active")) {
		t.Fatalf("activation failure hidden: %s", out)
	}
}
