package memory

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Confirmations were a per-process map, so the two failures these
// tests are about were both reachable: an approval arriving at a node
// that never asked, and a restart between question and answer.

// newPromptStack returns a factory so a test can mint several stores
// against one raft cluster — the stand-in for several nodes.
func newPromptStack(t *testing.T) func(ttl time.Duration) *PromptStore {
	t.Helper()
	dataDir := t.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dataDir, "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	fsm := NewFSM(store)
	_, inmem := raft.NewInmemTransport("prompt-node")
	node, err := NewRaft(RaftConfig{
		NodeID: "prompt-node", LocalAddr: "prompt-node",
		DataDir: dataDir, Bootstrap: true, Transport: inmem,
	}, fsm)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = node.Shutdown()
		_ = store.Close()
	})
	if err := node.WaitForLeader(5 * time.Second); err != nil {
		t.Fatal(err)
	}

	return func(ttl time.Duration) *PromptStore {
		p, err := NewPromptStore(PromptStoreConfig{Raft: node, Store: store, TTL: ttl})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
}

func pending(reason string) *lobslawv1.PromptRecord {
	return &lobslawv1.PromptRecord{
		Reason:    reason,
		Channel:   "telegram",
		ChannelId: "-100",
		Action:    "storage.write",
		Resource:  "notes/plan.md",
	}
}

// The whole point of moving this into raft: a user approving on their
// phone while a second tap lands from the desktop must produce one
// decision, not two, even when the two arrive at different nodes.
func TestConcurrentResolveHasExactlyOneWinner(t *testing.T) {
	t.Parallel()
	newStore := newPromptStack(t)
	author := newStore(time.Minute)

	rec, err := author.Create(pending("write to your notes?"))
	if err != nil {
		t.Fatal(err)
	}

	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
		other   []error
	)
	start := make(chan struct{})
	for i := range racers {
		wg.Go(func() {
			resolver := newStore(time.Minute)
			by := fmt.Sprintf("node-%d", i)
			<-start
			out, err := resolver.Resolve(rec.Id,
				lobslawv1.PromptDecision_PROMPT_DECISION_APPROVED,
				lobslawv1.PromptScope_PROMPT_SCOPE_ONCE, by)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners = append(winners, out.ResolvedBy)
			case errors.Is(err, ErrPromptResolved):
				// expected for everyone but one
			default:
				other = append(other, err)
			}
		})
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Errorf("losers should report ErrPromptResolved, got: %v", other)
	}
	if len(winners) != 1 {
		t.Fatalf("%d resolvers won, want exactly 1: %v", len(winners), winners)
	}

	final, err := author.Get(rec.Id)
	if err != nil {
		t.Fatal(err)
	}
	if final.ResolvedBy != winners[0] {
		t.Errorf("stored resolver %q, but %q was told it won", final.ResolvedBy, winners[0])
	}
	if final.Decision != lobslawv1.PromptDecision_PROMPT_DECISION_APPROVED {
		t.Errorf("decision = %v, want APPROVED", final.Decision)
	}
}

// Ask on one node, answer on another. Under the old map this returned
// "unknown prompt" and the turn was lost.
func TestAnotherNodeCanAnswer(t *testing.T) {
	t.Parallel()
	newStore := newPromptStack(t)
	a, b := newStore(time.Minute), newStore(time.Minute)

	rec, err := a.Create(pending("send this email?"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Resolve(rec.Id,
		lobslawv1.PromptDecision_PROMPT_DECISION_DENIED,
		lobslawv1.PromptScope_PROMPT_SCOPE_ONCE, "node-b"); err != nil {
		t.Fatalf("node B could not answer node A's question: %v", err)
	}

	seen, err := a.Get(rec.Id)
	if err != nil {
		t.Fatal(err)
	}
	if seen.Decision != lobslawv1.PromptDecision_PROMPT_DECISION_DENIED {
		t.Errorf("node A sees %v, want DENIED", seen.Decision)
	}
}

// The continuation is what makes resuming elsewhere possible at all: a
// decision with no turn to resume is just a logged opinion.
func TestContinuationSurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()
	newStore := newPromptStack(t)
	a, b := newStore(time.Minute), newStore(time.Minute)

	rec := pending("run this?")
	rec.TurnId = "turn-7"
	rec.SessionId = "sess-3"
	rec.Continuation = &lobslawv1.Continuation{
		Messages: []*lobslawv1.SessionMessage{
			{Role: "user", Content: "tidy my notes"},
			{Role: "assistant", Content: "I'll need to write to notes/plan.md."},
		},
		SpentUsd:     0.0125,
		ToolCalls:    3,
		LlmCalls:     2,
		SystemPrompt: "you are lobslaw",
	}
	created, err := a.Create(rec)
	if err != nil {
		t.Fatal(err)
	}

	got, err := b.Get(created.Id)
	if err != nil {
		t.Fatal(err)
	}
	c := got.Continuation
	if c == nil {
		t.Fatal("continuation lost; the turn cannot resume on another node")
	}
	if len(c.Messages) != 2 || c.Messages[1].Content != "I'll need to write to notes/plan.md." {
		t.Errorf("transcript did not round-trip: %+v", c.Messages)
	}
	if c.ToolCalls != 3 || c.LlmCalls != 2 {
		t.Errorf("budget counters lost: tool=%d llm=%d", c.ToolCalls, c.LlmCalls)
	}
	if c.SpentUsd != 0.0125 {
		t.Errorf("spend lost: %v — resuming would reset the budget", c.SpentUsd)
	}
	if got.TurnId != "turn-7" || got.SessionId != "sess-3" {
		t.Errorf("turn identity lost: turn=%q session=%q", got.TurnId, got.SessionId)
	}
}

// A timer dies with its process, so an unanswered prompt on a node
// that then restarts would sit pending forever. The sweeper is state,
// not a timer.
func TestSweepTimesOutOnlyExpiredPending(t *testing.T) {
	t.Parallel()
	newStore := newPromptStack(t)
	s := newStore(time.Minute)

	now := time.Now()
	mk := func(reason string, expiry time.Time) *lobslawv1.PromptRecord {
		rec := pending(reason)
		rec.ExpiresAt = timestamppb.New(expiry)
		out, err := s.Create(rec)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	stale := mk("stale", now.Add(-time.Minute))
	fresh := mk("fresh", now.Add(time.Hour))
	answered := mk("answered", now.Add(-time.Minute))
	if _, err := s.Resolve(answered.Id,
		lobslawv1.PromptDecision_PROMPT_DECISION_APPROVED,
		lobslawv1.PromptScope_PROMPT_SCOPE_ONCE, "user"); err != nil {
		t.Fatal(err)
	}

	closed, err := s.Sweep(now)
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Errorf("swept %d prompts, want 1 (only the expired pending one)", closed)
	}

	for _, tc := range []struct {
		name string
		id   string
		want lobslawv1.PromptDecision
	}{
		{"expired and unanswered", stale.Id, lobslawv1.PromptDecision_PROMPT_DECISION_TIMED_OUT},
		{"not yet expired", fresh.Id, lobslawv1.PromptDecision_PROMPT_DECISION_PENDING},
		// Expired, but the user answered first. Their decision stands:
		// the sweeper closes unanswered questions, it does not revise
		// answered ones.
		{"expired but already answered", answered.Id, lobslawv1.PromptDecision_PROMPT_DECISION_APPROVED},
	} {
		got, err := s.Get(tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Decision != tc.want {
			t.Errorf("%s: decision = %v, want %v", tc.name, got.Decision, tc.want)
		}
	}

	if got, _ := s.Get(answered.Id); got.ResolvedBy != "user" {
		t.Errorf("the sweeper replaced the resolver: %q", got.ResolvedBy)
	}
}

// An unexpired prompt must survive a sweep, or every confirmation
// would be closed the moment any node ran the sweeper.
func TestSweepLeavesUnexpiredAlone(t *testing.T) {
	t.Parallel()
	newStore := newPromptStack(t)
	s := newStore(time.Hour)

	rec, err := s.Create(pending("plenty of time"))
	if err != nil {
		t.Fatal(err)
	}
	closed, err := s.Sweep(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if closed != 0 {
		t.Errorf("swept %d prompts that had not expired", closed)
	}
	got, err := s.Get(rec.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != lobslawv1.PromptDecision_PROMPT_DECISION_PENDING {
		t.Errorf("decision = %v, want still PENDING", got.Decision)
	}
}

// An explicit expiry the caller set must be honoured rather than
// overwritten by the store's default TTL.
func TestCallerExpiryWins(t *testing.T) {
	t.Parallel()
	newStore := newPromptStack(t)
	s := newStore(time.Hour)

	want := time.Now().Add(90 * time.Second).Truncate(time.Second)
	rec := pending("short fuse")
	rec.ExpiresAt = timestamppb.New(want)
	created, err := s.Create(rec)
	if err != nil {
		t.Fatal(err)
	}
	if got := created.ExpiresAt.AsTime().Truncate(time.Second); !got.Equal(want) {
		t.Errorf("expiry = %v, want %v — the default TTL overwrote it", got, want)
	}
}

// Wait is how the asking turn learns the answer; the resolution
// arrives from a different store, as it would from another node.
func TestWaitSeesARemoteResolution(t *testing.T) {
	t.Parallel()
	newStore := newPromptStack(t)
	asker, answerer := newStore(time.Minute), newStore(time.Minute)

	rec, err := asker.Create(pending("go ahead?"))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = answerer.Resolve(rec.Id,
			lobslawv1.PromptDecision_PROMPT_DECISION_APPROVED,
			lobslawv1.PromptScope_PROMPT_SCOPE_SESSION, "phone")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := asker.Wait(ctx, rec.Id, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("Wait never saw the remote answer: %v", err)
	}
	if got.Decision != lobslawv1.PromptDecision_PROMPT_DECISION_APPROVED {
		t.Errorf("decision = %v, want APPROVED", got.Decision)
	}
	if got.Scope != lobslawv1.PromptScope_PROMPT_SCOPE_SESSION {
		t.Errorf("scope = %v, want SESSION", got.Scope)
	}
}

// A caller that never answers must not leave the turn blocked forever.
func TestWaitHonoursContext(t *testing.T) {
	t.Parallel()
	newStore := newPromptStack(t)
	s := newStore(time.Minute)

	rec, err := s.Create(pending("nobody is home"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.Wait(ctx, rec.Id, 10*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait returned %v, want the context deadline", err)
	}
}

// PENDING is not an answer. Accepting it would let a caller "resolve"
// a prompt into the state it is already in, burning the one CAS the
// real answer needed.
func TestResolveRejectsNonDecisions(t *testing.T) {
	t.Parallel()
	newStore := newPromptStack(t)
	s := newStore(time.Minute)

	rec, err := s.Create(pending("x"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []lobslawv1.PromptDecision{
		lobslawv1.PromptDecision_PROMPT_DECISION_PENDING,
		lobslawv1.PromptDecision_PROMPT_DECISION_UNSPECIFIED,
	} {
		if _, err := s.Resolve(rec.Id, d, lobslawv1.PromptScope_PROMPT_SCOPE_ONCE, "u"); err == nil {
			t.Errorf("%v was accepted as a resolution", d)
		}
	}
}

func TestUnknownPromptIsNotFound(t *testing.T) {
	t.Parallel()
	newStore := newPromptStack(t)
	s := newStore(time.Minute)

	if _, err := s.Get("no-such-id"); !errors.Is(err, ErrPromptNotFound) {
		t.Errorf("Get returned %v, want ErrPromptNotFound", err)
	}
	if _, err := s.Resolve("no-such-id",
		lobslawv1.PromptDecision_PROMPT_DECISION_APPROVED,
		lobslawv1.PromptScope_PROMPT_SCOPE_ONCE, "u"); !errors.Is(err, ErrPromptNotFound) {
		t.Errorf("Resolve returned %v, want ErrPromptNotFound", err)
	}
}

// A raft failure is not the same as losing a race. Reporting it as
// ErrPromptResolved would tell the user somebody else decided when
// nobody had.
func TestRaftFailureIsNotReportedAsResolved(t *testing.T) {
	t.Parallel()
	newStore := newPromptStack(t)
	real := newStore(time.Minute)

	rec, err := real.Create(pending("x"))
	if err != nil {
		t.Fatal(err)
	}

	broken, err := NewPromptStore(PromptStoreConfig{
		Raft:  failingApplier{},
		Store: real.store,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = broken.Resolve(rec.Id,
		lobslawv1.PromptDecision_PROMPT_DECISION_APPROVED,
		lobslawv1.PromptScope_PROMPT_SCOPE_ONCE, "u")
	if err == nil {
		t.Fatal("a failed apply reported success")
	}
	if errors.Is(err, ErrPromptResolved) {
		t.Errorf("a raft failure was reported as a lost race: %v", err)
	}
}

type failingApplier struct{}

func (failingApplier) Apply([]byte, time.Duration) (any, error) {
	return nil, errors.New("no leader")
}
