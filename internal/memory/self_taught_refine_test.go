package memory

import (
	"context"
	"errors"
	"strings"
	"testing"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Two failures this closes.
//
// A refinement used to overwrite the working artefact and knock it
// back to PROPOSED, so a skill used successfully for a month stopped
// loading because the agent had an idea about improving it.
//
// And identity was exact-name only, so a proposer that varied the name
// slightly produced a second artefact for one job — two entries in the
// index contradicting each other about how to do the same task, and
// nothing downstream with any basis to reconcile them.

func named(name, desc, body string) *lobslawv1.SelfTaughtRecord {
	return &lobslawv1.SelfTaughtRecord{
		Kind:        lobslawv1.SelfTaughtKind_SELF_TAUGHT_KIND_SKILL,
		Name:        name,
		Description: desc,
		Body:        body,
		Owner:       "user:alice",
	}
}

// The bug: an active artefact must keep working while a refinement
// waits for approval.
func TestRefinementDoesNotDisplaceTheActiveVersion(t *testing.T) {
	t.Parallel()
	s := selfTaught(t, SelfLearningPropose)
	ctx := context.Background()

	if _, err := s.Propose(ctx, named("tidy-notes", "tidies notes", "v1 body"), ProposeIntent{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(ctx, "skill:tidy-notes", "alice"); err != nil {
		t.Fatal(err)
	}

	refined, err := s.Propose(ctx, named("tidy-notes", "tidies notes better", "v2 body"), ProposeIntent{
		Refines:   "skill:tidy-notes",
		Rationale: "handles nested folders",
	})
	if err != nil {
		t.Fatal(err)
	}

	if refined.State != lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_ACTIVE {
		t.Errorf("state = %v; a refinement took the working skill out of service", refined.State)
	}
	if refined.Body != "v1 body" {
		t.Errorf("body = %q; the refinement overwrote the version in force", refined.Body)
	}
	if refined.Pending == nil {
		t.Fatal("no pending revision; the refinement was lost")
	}
	if refined.Pending.Body != "v2 body" {
		t.Errorf("pending body = %q", refined.Pending.Body)
	}
	if refined.Pending.Rationale != "handles nested folders" {
		t.Errorf("rationale lost: %q", refined.Pending.Rationale)
	}

	// And it is still the loadable one.
	active, err := s.Active(lobslawv1.SelfTaughtKind_SELF_TAUGHT_KIND_SKILL)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].Body != "v1 body" {
		t.Errorf("active set = %+v; the working version is not loadable", active)
	}
}

func TestApprovingAPendingRevisionSwapsItIn(t *testing.T) {
	t.Parallel()
	s := selfTaught(t, SelfLearningPropose)
	ctx := context.Background()
	if _, err := s.Propose(ctx, named("tidy-notes", "tidies notes", "v1"), ProposeIntent{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(ctx, "skill:tidy-notes", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Propose(ctx, named("tidy-notes", "better", "v2"), ProposeIntent{
		Refines: "skill:tidy-notes", Rationale: "why",
	}); err != nil {
		t.Fatal(err)
	}

	out, err := s.ApprovePending(ctx, "skill:tidy-notes", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if out.Body != "v2" {
		t.Errorf("body = %q, want the refinement", out.Body)
	}
	if out.Description != "better" {
		t.Errorf("description = %q; the refinement's description was dropped", out.Description)
	}
	if out.Pending != nil {
		t.Error("the pending revision survived approval")
	}
	if out.Version != 2 {
		t.Errorf("version = %d, want 2", out.Version)
	}
}

// Rejecting must leave the live version untouched — that is the whole
// safety property of staging.
func TestRejectingAPendingRevisionChangesNothing(t *testing.T) {
	t.Parallel()
	s := selfTaught(t, SelfLearningPropose)
	ctx := context.Background()
	if _, err := s.Propose(ctx, named("tidy-notes", "tidies", "v1"), ProposeIntent{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(ctx, "skill:tidy-notes", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Propose(ctx, named("tidy-notes", "worse", "v2"), ProposeIntent{
		Refines: "skill:tidy-notes", Rationale: "why",
	}); err != nil {
		t.Fatal(err)
	}

	out, err := s.RejectPending(ctx, "skill:tidy-notes")
	if err != nil {
		t.Fatal(err)
	}
	if out.Body != "v1" || out.Description != "tidies" {
		t.Errorf("rejecting altered the live version: %q / %q", out.Body, out.Description)
	}
	if out.Pending != nil {
		t.Error("the rejected revision is still staged")
	}
	if out.Version != 1 {
		t.Errorf("version = %d; rejecting bumped it", out.Version)
	}
}

func TestApproveOrRejectWithNothingPending(t *testing.T) {
	t.Parallel()
	s := selfTaught(t, SelfLearningAuto)
	ctx := context.Background()
	if _, err := s.Propose(ctx, named("thing", "d", "b"), ProposeIntent{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApprovePending(ctx, "skill:thing", "alice"); !errors.Is(err, ErrNoPendingRevision) {
		t.Errorf("err = %v, want ErrNoPendingRevision", err)
	}
	if _, err := s.RejectPending(ctx, "skill:thing"); !errors.Is(err, ErrNoPendingRevision) {
		t.Errorf("err = %v, want ErrNoPendingRevision", err)
	}
}

// A diff with no reasoning is one nobody can approve with any
// confidence.
func TestRefinementNeedsARationale(t *testing.T) {
	t.Parallel()
	s := selfTaught(t, SelfLearningPropose)
	ctx := context.Background()
	if _, err := s.Propose(ctx, named("thing", "d", "v1"), ProposeIntent{}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Propose(ctx, named("thing", "d", "v2"), ProposeIntent{Refines: "skill:thing"})
	if err == nil {
		t.Fatal("a refinement with no rationale was accepted")
	}
	if !strings.Contains(err.Error(), "rationale") {
		t.Errorf("err = %q", err)
	}
}

// In auto mode there is nothing to wait for, so a refinement applies —
// that is what the operator asked for by choosing auto.
func TestAutoModeAppliesRefinementsDirectly(t *testing.T) {
	t.Parallel()
	s := selfTaught(t, SelfLearningAuto)
	ctx := context.Background()
	if _, err := s.Propose(ctx, named("thing", "d", "v1"), ProposeIntent{}); err != nil {
		t.Fatal(err)
	}
	out, err := s.Propose(ctx, named("thing", "d2", "v2"), ProposeIntent{
		Refines: "skill:thing", Rationale: "better",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body != "v2" || out.Pending != nil {
		t.Errorf("auto mode staged instead of applying: body=%q pending=%v", out.Body, out.Pending)
	}
	if out.Version != 2 {
		t.Errorf("version = %d, want 2", out.Version)
	}
}

// A proposer that reuses the exact name is refining whether or not it
// noticed, so it routes as one rather than silently overwriting.
func TestExactNameCollisionRoutesAsARefinement(t *testing.T) {
	t.Parallel()
	s := selfTaught(t, SelfLearningPropose)
	ctx := context.Background()
	if _, err := s.Propose(ctx, named("thing", "d", "v1"), ProposeIntent{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(ctx, "skill:thing", "alice"); err != nil {
		t.Fatal(err)
	}

	out, err := s.Propose(ctx, named("thing", "d", "v2"), ProposeIntent{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body != "v1" {
		t.Errorf("the live body was overwritten: %q", out.Body)
	}
	if out.Pending == nil || out.Pending.Body != "v2" {
		t.Errorf("the re-proposal was not staged: %+v", out.Pending)
	}
}

// --- the near-duplicate guard --------------------------------------

// The case exact-name matching misses entirely.
func TestNearDuplicateNameIsRefused(t *testing.T) {
	t.Parallel()
	s := selfTaught(t, SelfLearningPropose)
	ctx := context.Background()
	if _, err := s.Propose(ctx, named("tidy-notes", "tidies my notes", "v1"), ProposeIntent{}); err != nil {
		t.Fatal(err)
	}

	_, err := s.Propose(ctx, named("tidying-note", "tidies my notes", "v1"), ProposeIntent{})
	if !errors.Is(err, ErrSimilarExists) {
		t.Fatalf("err = %v, want ErrSimilarExists — a near-duplicate slipped through", err)
	}

	// The refusal has to carry the candidates, or the proposer needs a
	// second round trip to find out what it collided with.
	var simErr *SimilarityError
	if !errors.As(err, &simErr) {
		t.Fatalf("err = %T, want a *SimilarityError carrying candidates", err)
	}
	if len(simErr.Candidates) == 0 || simErr.Candidates[0].Record.Id != "skill:tidy-notes" {
		t.Errorf("candidates = %+v", simErr.Candidates)
	}
	if !strings.Contains(err.Error(), "skill:tidy-notes") {
		t.Errorf("the message does not name what it collided with: %q", err)
	}
}

// Declaring it distinct is how a proposer that has looked proceeds.
func TestDistinctDeclarationAllowsANearDuplicate(t *testing.T) {
	t.Parallel()
	s := selfTaught(t, SelfLearningPropose)
	ctx := context.Background()
	if _, err := s.Propose(ctx, named("tidy-notes", "tidies my notes", "v1"), ProposeIntent{}); err != nil {
		t.Fatal(err)
	}

	out, err := s.Propose(ctx, named("tidying-note", "tidies my notes", "v1"),
		ProposeIntent{Distinct: true})
	if err != nil {
		t.Fatalf("a declared-distinct artefact was refused: %v", err)
	}
	if out.Id != "skill:tidying-note" {
		t.Errorf("id = %q", out.Id)
	}
	live, _ := s.List(SelfTaughtQuery{})
	if len(live) != 2 {
		t.Errorf("live = %d artefacts, want both", len(live))
	}
}

// Genuinely unrelated artefacts must not be caught. A check that
// refuses everything gets turned off, and then it protects nothing.
func TestUnrelatedArtefactsPassFreely(t *testing.T) {
	t.Parallel()
	s := selfTaught(t, SelfLearningPropose)
	ctx := context.Background()
	if _, err := s.Propose(ctx, named("tidy-notes", "tidies my notes", "v1"), ProposeIntent{}); err != nil {
		t.Fatal(err)
	}

	for _, rec := range []*lobslawv1.SelfTaughtRecord{
		named("deploy-staging", "pushes a build to staging", "b"),
		named("summarise-pdfs", "extracts the gist of a PDF", "b"),
		named("check-certs", "reports expiring TLS certificates", "b"),
	} {
		if _, err := s.Propose(ctx, rec, ProposeIntent{}); err != nil {
			t.Errorf("unrelated artefact %q was refused: %v", rec.Name, err)
		}
	}
}

// Different kinds are different namespaces; a procedure named like a
// skill is not a duplicate of it.
func TestSimilarityIsScopedToTheKind(t *testing.T) {
	t.Parallel()
	s := selfTaught(t, SelfLearningPropose)
	ctx := context.Background()
	if _, err := s.Propose(ctx, named("tidy-notes", "tidies notes", "v1"), ProposeIntent{}); err != nil {
		t.Fatal(err)
	}

	proc := named("tidy-notes", "tidies notes", "v1")
	proc.Kind = lobslawv1.SelfTaughtKind_SELF_TAUGHT_KIND_PROCEDURE
	if _, err := s.Propose(ctx, proc, ProposeIntent{}); err != nil {
		t.Errorf("a procedure was refused for colliding with a skill: %v", err)
	}
}

// The semantic half: a rename with no shared tokens is invisible to
// lexical scoring and has to be caught by meaning.
func TestEmbedderCatchesARenameLexicalMisses(t *testing.T) {
	t.Parallel()
	s := selfTaught(t, SelfLearningPropose)
	s.SetEmbedder(fakeEmbedder{
		"tidy-notes":          {1, 0, 0},
		"organise-scratchpad": {0.99, 0.14, 0},
		"deploy-staging":      {0, 1, 0},
	})
	ctx := context.Background()
	if _, err := s.Propose(ctx, named("tidy-notes", "aaa", "b"), ProposeIntent{}); err != nil {
		t.Fatal(err)
	}

	// No shared tokens at all, so lexical scores zero.
	if got := lexicalSimilarity(named("organise-scratchpad", "zzz", ""), named("tidy-notes", "aaa", "")); got != 0 {
		t.Fatalf("setup: lexical scored %v, so this test proves nothing about embeddings", got)
	}

	_, err := s.Propose(ctx, named("organise-scratchpad", "zzz", "b"), ProposeIntent{})
	if !errors.Is(err, ErrSimilarExists) {
		t.Errorf("err = %v; the semantic half did not catch a rename", err)
	}

	// And something genuinely unrelated still passes.
	if _, err := s.Propose(ctx, named("deploy-staging", "yyy", "b"), ProposeIntent{}); err != nil {
		t.Errorf("an unrelated artefact was refused: %v", err)
	}
}

// A configured embedder that fails must not silently downgrade to
// lexical. The check exists to stop duplicates, and quietly skipping
// half of it produces exactly what it was wired to prevent.
func TestEmbedderFailureIsNotSilentlyIgnored(t *testing.T) {
	t.Parallel()
	s := selfTaught(t, SelfLearningPropose)
	s.SetEmbedder(brokenEmbedder{})
	_, err := s.Propose(context.Background(), named("thing", "d", "b"), ProposeIntent{})
	if err == nil {
		t.Fatal("a broken embedder was silently skipped")
	}
	if !strings.Contains(err.Error(), "embedder unavailable") {
		t.Errorf("err = %q; it does not surface the embedder failure", err)
	}
}

// --- fakes ---------------------------------------------------------

// fakeEmbedder maps a name to a fixed vector, so a test can state
// which artefacts are semantically close without an LLM.
type fakeEmbedder map[string][]float32

func (f fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	name := strings.SplitN(text, "\n", 2)[0]
	if v, ok := f[name]; ok {
		return v, nil
	}
	return []float32{0, 0, 1}, nil
}

type brokenEmbedder struct{}

func (brokenEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, errors.New("embedder unavailable")
}

func (fakeEmbedder) Model() string   { return "test-embedder-v1" }
func (brokenEmbedder) Model() string { return "test-embedder-v1" }
