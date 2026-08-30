package compute

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// The fork spends real tokens and writes instructions the agent will
// later follow, so most of what matters here is about when it does NOT
// run and what it must not do.

type recordingStore struct {
	mu       sync.Mutex
	existing []ArtefactSummary
	proposed []ProposedArtefact
	failWith error
}

func (s *recordingStore) Existing(string) ([]ArtefactSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.existing, nil
}

func (s *recordingStore) Propose(_ context.Context, a ProposedArtefact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failWith != nil {
		return s.failWith
	}
	s.proposed = append(s.proposed, a)
	return nil
}

func (s *recordingStore) calls() []ProposedArtefact {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ProposedArtefact(nil), s.proposed...)
}

// reviewProvider returns one fixed answer and records what it was
// asked.
type reviewProvider struct {
	mu       sync.Mutex
	reply    string
	err      error
	requests []ChatRequest
}

func (p *reviewProvider) Chat(_ context.Context, req ChatRequest) (*ChatResponse, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	return &ChatResponse{Content: p.reply}, nil
}

func (p *reviewProvider) seen() []ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ChatRequest(nil), p.requests...)
}

func decisionJSON(t *testing.T, d reviewDecision) string {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func newFork(t *testing.T, cfg ReviewConfig) (*ReviewFork, *recordingStore, *reviewProvider) {
	t.Helper()
	provider := &reviewProvider{reply: `{"action":"none"}`}
	store := &recordingStore{}
	roles, err := NewRoleMap(provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Roles, cfg.Store, cfg.Logger = roles, store, slog.New(slog.DiscardHandler)
	f, err := NewReviewFork(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return f, store, provider
}

func aTurn() ProcessMessageRequest {
	return ProcessMessageRequest{
		TurnID: "t1", Channel: "telegram", ChannelID: "-100",
		Claims: &types.Claims{UserID: "alice"},
	}
}

// --- when it must not run ------------------------------------------

// No human in the loop means nothing to learn about the user, and the
// fork is expensive enough that spending it on a cron tick is the
// wrong trade twice over.
func TestSchedulerTurnsAreNotReviewed(t *testing.T) {
	t.Parallel()
	f, _, _ := newFork(t, ReviewConfig{SkillToolIterations: 1})
	req := aTurn()
	req.Channel, req.ChannelID = "", "" // scheduler origin

	if f.shouldReview(req, 50).any() {
		t.Error("a scheduler turn triggered a review")
	}
}

// Without this the first review triggers the second, and a review of a
// review is meaningless and unbounded.
func TestAForkCannotSpawnAFork(t *testing.T) {
	t.Parallel()
	f, _, _ := newFork(t, ReviewConfig{SkillToolIterations: 1, MemoryTurnInterval: 1})
	req := aTurn()
	req.IsReviewFork = true

	if f.shouldReview(req, 50).any() {
		t.Error("a review fork triggered another review")
	}
}

// --- the asymmetric triggers ---------------------------------------

// A skill answers "was there enough work here", which is a property of
// one turn.
func TestSkillsTriggerOnToolVolumeInOneTurn(t *testing.T) {
	t.Parallel()
	f, _, _ := newFork(t, ReviewConfig{SkillToolIterations: 10, MemoryTurnInterval: -1})

	if f.shouldReview(aTurn(), 9).skills {
		t.Error("9 tool calls triggered a skill review")
	}
	if !f.shouldReview(aTurn(), 10).skills {
		t.Error("10 tool calls did not trigger a skill review")
	}
	// And one enormous turn is enough on its own — it does not have to
	// wait for a turn count.
	if !f.shouldReview(aTurn(), 40).skills {
		t.Error("40 tool calls did not trigger immediately")
	}
}

// Memory answers "have we learned who this person is", which only
// accumulates across turns. Forty turns of chat produce no procedure;
// one enormous turn teaches nothing about the user.
func TestMemoryTriggersOnTurnCountNotToolVolume(t *testing.T) {
	t.Parallel()
	f, _, _ := newFork(t, ReviewConfig{SkillToolIterations: -1, MemoryTurnInterval: 3})

	// A single huge turn is not a pattern.
	if f.shouldReview(aTurn(), 100).memory {
		t.Error("one turn with 100 tool calls triggered a memory review")
	}
	// Two more chatty turns reach the interval.
	f.shouldReview(aTurn(), 0)
	if !f.shouldReview(aTurn(), 0).memory {
		t.Error("the memory interval never fired")
	}
	// And it resets, rather than firing on every turn after.
	if f.shouldReview(aTurn(), 0).memory {
		t.Error("the memory counter did not reset")
	}
}

// Conversations count separately: a busy chat must not trigger a
// review of a quiet one.
func TestMemoryCountIsPerConversation(t *testing.T) {
	t.Parallel()
	f, _, _ := newFork(t, ReviewConfig{SkillToolIterations: -1, MemoryTurnInterval: 2})

	busy := aTurn()
	quiet := aTurn()
	quiet.ChannelID = "-200"

	f.shouldReview(busy, 0)
	if f.shouldReview(quiet, 0).memory {
		t.Error("a different conversation inherited the first one's count")
	}
	if !f.shouldReview(busy, 0).memory {
		t.Error("the busy conversation did not reach its interval")
	}
}

func TestNegativeIntervalDisablesAnAxis(t *testing.T) {
	t.Parallel()
	f, _, _ := newFork(t, ReviewConfig{SkillToolIterations: -1, MemoryTurnInterval: -1})
	if f.shouldReview(aTurn(), 1000).any() {
		t.Error("both axes disabled and a review still fired")
	}
}

// --- what it proposes ----------------------------------------------

func TestNothingLearnedProposesNothing(t *testing.T) {
	t.Parallel()
	f, store, _ := newFork(t, ReviewConfig{SkillToolIterations: 1})
	if err := f.run(context.Background(), aTurn(), nil, reviewAxes{skills: true}); err != nil {
		t.Fatal(err)
	}
	if got := store.calls(); len(got) != 0 {
		t.Errorf("a quiet pass proposed %+v", got)
	}
}

func TestANewSkillIsProposed(t *testing.T) {
	t.Parallel()
	f, store, provider := newFork(t, ReviewConfig{SkillToolIterations: 1})
	provider.reply = decisionJSON(t, reviewDecision{
		Action: "new", Name: "rotate-certs", Description: "renews expiring certs",
		Body: "steps", Distinct: true,
	})

	if err := f.run(context.Background(), aTurn(), nil, reviewAxes{skills: true}); err != nil {
		t.Fatal(err)
	}
	got := store.calls()
	if len(got) != 1 {
		t.Fatalf("proposed %d artefacts", len(got))
	}
	if got[0].Name != "rotate-certs" || !got[0].Distinct {
		t.Errorf("proposal = %+v", got[0])
	}
	if got[0].TurnID != "t1" || got[0].Owner != "user:alice" {
		t.Errorf("provenance lost: turn=%q owner=%q", got[0].TurnID, got[0].Owner)
	}
}

func TestARefinementCarriesItsTargetAndRationale(t *testing.T) {
	t.Parallel()
	f, store, provider := newFork(t, ReviewConfig{SkillToolIterations: 1})
	provider.reply = decisionJSON(t, reviewDecision{
		Action: "refine", Refines: "skill:rotate-certs", Name: "rotate-certs",
		Body: "better steps", Rationale: "handles wildcard certs",
	})

	if err := f.run(context.Background(), aTurn(), nil, reviewAxes{skills: true}); err != nil {
		t.Fatal(err)
	}
	got := store.calls()
	if len(got) != 1 {
		t.Fatalf("proposed %d artefacts", len(got))
	}
	if got[0].Refines != "skill:rotate-certs" || got[0].Rationale == "" {
		t.Errorf("proposal = %+v", got[0])
	}
	if got[0].Distinct {
		t.Error("a refinement was marked distinct")
	}
}

// A malformed answer must not become an artefact. Retrying, or
// guessing at the intent, puts a badly-formed instruction into the
// store on the strength of a response the model could not even shape.
func TestUnparseableDecisionLearnsNothing(t *testing.T) {
	t.Parallel()
	for _, reply := range []string{
		"I think we should remember that the user likes tea.",
		`{"action":"maybe"}`,
		`{"action":"refine"}`, // no target
		"",
		`{"action":`,
	} {
		f, store, provider := newFork(t, ReviewConfig{SkillToolIterations: 1})
		provider.reply = reply
		if err := f.run(context.Background(), aTurn(), nil, reviewAxes{skills: true}); err != nil {
			t.Errorf("reply %q returned an error rather than learning nothing: %v", reply, err)
		}
		if got := store.calls(); len(got) != 0 {
			t.Errorf("reply %q produced %+v", reply, got)
		}
	}
}

// Models habitually wrap JSON in a fence; refusing that would make
// every review a quiet pass for a reason that has nothing to do with
// the conversation.
func TestFencedJSONIsAccepted(t *testing.T) {
	t.Parallel()
	f, store, provider := newFork(t, ReviewConfig{SkillToolIterations: 1})
	provider.reply = "```json\n{\"action\":\"new\",\"name\":\"x\",\"body\":\"b\",\"distinct\":true}\n```"

	if err := f.run(context.Background(), aTurn(), nil, reviewAxes{skills: true}); err != nil {
		t.Fatal(err)
	}
	if got := store.calls(); len(got) != 1 {
		t.Errorf("a fenced decision was discarded: %+v", got)
	}
}

// A refused proposal is logged and dropped, never retried. Asking
// again costs another replay to maybe produce a marginal artefact that
// propose mode would then make somebody approve.
func TestARefusedProposalIsNotRetried(t *testing.T) {
	t.Parallel()
	f, store, provider := newFork(t, ReviewConfig{SkillToolIterations: 1})
	provider.reply = decisionJSON(t, reviewDecision{Action: "new", Name: "x", Body: "b"})
	store.failWith = errors.New("a similar artefact already exists")

	if err := f.run(context.Background(), aTurn(), nil, reviewAxes{skills: true}); err != nil {
		t.Errorf("a refusal was surfaced as a fork error: %v", err)
	}
	if n := len(provider.seen()); n != 1 {
		t.Errorf("the model was called %d times; the fork retried", n)
	}
}

// --- the prompt ----------------------------------------------------

// The fork is shown the complete list so it can name a refinement
// target. A filtered view produces duplicates of whatever the filter
// missed, which is the failure this list exists to prevent.
func TestThePromptCarriesEveryExistingSkill(t *testing.T) {
	t.Parallel()
	f, store, provider := newFork(t, ReviewConfig{SkillToolIterations: 1})
	store.existing = []ArtefactSummary{
		{ID: "skill:a", Name: "a", Description: "does a"},
		{ID: "skill:b", Name: "b", Description: "does b"},
	}

	if err := f.run(context.Background(), aTurn(), nil, reviewAxes{skills: true}); err != nil {
		t.Fatal(err)
	}
	seen := provider.seen()
	if len(seen) != 1 {
		t.Fatalf("called %d times", len(seen))
	}
	prompt := seen[0].Messages[0].Content
	for _, want := range []string{"skill:a", "skill:b", "complete"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt is missing %q:\n%s", want, prompt)
		}
	}
}

// The naming rule and the preference order are the two things that
// stop a library becoming a pile of session artefacts.
func TestThePromptCarriesTheAntiSprawlRules(t *testing.T) {
	t.Parallel()
	prompt := reviewPrompt(reviewAxes{skills: true}, nil)
	for _, want := range []string{
		"Refine a skill that already exists",
		"issue or PR number",
		"Class-level names only",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not carry %q", want)
		}
	}
	// And the conservative bias, which is where this deliberately
	// diverges from hermes.
	if !strings.Contains(prompt, "quiet pass is a correct and common outcome") {
		t.Error("the prompt does not tell the model that learning nothing is fine")
	}
}

// A turn that qualified only on tool volume must not be asked to
// speculate about who the user is from one exchange.
func TestThePromptNamesWhatTriggeredIt(t *testing.T) {
	t.Parallel()
	skills := reviewPrompt(reviewAxes{skills: true}, nil)
	if !strings.Contains(skills, "do not speculate about the user") {
		t.Errorf("a skills-only review invites speculation about the user:\n%s", skills)
	}
	memory := reviewPrompt(reviewAxes{memory: true}, nil)
	if !strings.Contains(memory, "a single exchange is not a pattern") {
		t.Errorf("a memory review does not warn against over-reading:\n%s", memory)
	}
}

// --- replay mode ---------------------------------------------------

// Same model means a warm prefix cache, so the full transcript is
// mostly cache reads. A different model cannot reuse it, so replaying
// everything would cold-write the lot.
func TestReplayIsFullOnTheMainModelAndDigestedOtherwise(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", digestMessageChars*2)
	messages := []Message{
		{Role: "system", Content: "the assistant's own instructions"},
		{Role: "user", Content: long},
	}

	main := &reviewProvider{}
	roles, err := NewRoleMap(main, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &ReviewFork{cfg: ReviewConfig{Roles: roles}, log: slog.New(slog.DiscardHandler)}

	full := f.replayBody(messages, true)
	if !strings.Contains(full, long) {
		t.Error("the full replay truncated the transcript")
	}

	digest := f.replayBody(messages, false)
	if strings.Contains(digest, long) {
		t.Error("the digest carried the full transcript")
	}
	if !strings.Contains(digest, "truncated") {
		t.Error("the digest does not mark what it dropped")
	}

	// The system prompt is the assistant's own configuration;
	// reviewing it would have the fork learning from itself.
	for _, body := range []string{full, digest} {
		if strings.Contains(body, "the assistant's own instructions") {
			t.Error("the replay included the system prompt")
		}
	}
}

// A tool-call turn with no prose still matters to the skill axis: the
// tools ARE the procedure.
func TestToolOnlyTurnsSurviveTheReplay(t *testing.T) {
	t.Parallel()
	f := &ReviewFork{log: slog.New(slog.DiscardHandler)}
	body := f.replayBody([]Message{
		{Role: "assistant", ToolCalls: []ToolCall{{Name: "read_file"}, {Name: "write_file"}}},
	}, true)
	for _, want := range []string{"read_file", "write_file"} {
		if !strings.Contains(body, want) {
			t.Errorf("the replay dropped %q:\n%s", want, body)
		}
	}
}

// The routing predicate the replay mode derives from.
func TestIsMainReportsTheRouting(t *testing.T) {
	t.Parallel()
	main := &reviewProvider{}
	other := &reviewProvider{}

	sameModel, err := NewRoleMap(main, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !sameModel.IsMain(RoleReview) {
		t.Error("an unrouted review is not reported as running on main")
	}

	routed, err := NewRoleMap(main, map[Role]LLMProvider{RoleReview: other})
	if err != nil {
		t.Fatal(err)
	}
	if routed.IsMain(RoleReview) {
		t.Error("a review routed elsewhere is reported as running on main")
	}
}

// --- fire-and-forget -----------------------------------------------

// Consider must not block: the reply has gone out and nothing waits on
// the fork.
func TestConsiderDoesNotBlock(t *testing.T) {
	t.Parallel()
	f, _, provider := newFork(t, ReviewConfig{SkillToolIterations: 1})
	provider.err = errors.New("slow and failing")

	done := make(chan struct{})
	go func() {
		f.Consider(context.Background(), aTurn(), nil, 5)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Consider blocked the caller")
	}
}

// A nil fork is what self-learning being off looks like from the turn
// loop, and it must be silent rather than a panic.
func TestNilForkIsInert(t *testing.T) {
	t.Parallel()
	var f *ReviewFork
	f.Consider(context.Background(), aTurn(), nil, 100)
	if f.shouldReview(aTurn(), 100).any() {
		t.Error("a nil fork reported that a review should run")
	}
}
