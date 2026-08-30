package compute

import (
	"context"
	"errors"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"
)

// The cluster half of "approved for the rest of this conversation".
// A process-local map was defensible against restarts and was never
// defensible against a second node: the user answered in one
// conversation and got asked again because the next message landed
// somewhere else.

// fakeGrants stands in for the replicated store, so this package can
// exercise the wiring without a raft.
type fakeGrants struct {
	granted map[string]string // key -> grantedBy
	err     error
	calls   int
}

func newFakeGrants() *fakeGrants { return &fakeGrants{granted: map[string]string{}} }

func (f *fakeGrants) Grant(_ context.Context, sessionID, action, resource, by string) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.granted[sessionID+"|"+action+"|"+resource] = by
	return nil
}

func (f *fakeGrants) Granted(sessionID, action, resource string) bool {
	_, ok := f.granted[sessionID+"|"+action+"|"+resource]
	return ok
}

func turnCtx(channel, channelID string) context.Context {
	return turn.WithIdentity(context.Background(), turn.Identity{
		UserID:    "tg-alice",
		Principal: identity.Principal("user:alice"),
		Channel:   channel,
		ChannelID: channelID,
	})
}

func TestAGrantReachesTheDurableStore(t *testing.T) {
	t.Parallel()
	fake := newFakeGrants()
	s := NewSessionApprovals()
	s.SetDurable(fake)

	if !s.Grant(turnCtx("telegram", "42"), "tool:exec", "shell") {
		t.Fatal("the grant was not recorded")
	}
	if by := fake.granted["telegram:42|tool:exec|shell"]; by != "user:alice" {
		t.Errorf("durable store has %q; the grant did not replicate with its principal", by)
	}
}

// The case the whole change exists for: this process never saw the
// approval, and honours it anyway.
func TestAGrantFromElsewhereIsHonouredHere(t *testing.T) {
	t.Parallel()
	fake := newFakeGrants()
	fake.granted["telegram:42|tool:exec|shell"] = "user:alice"

	s := NewSessionApprovals()
	s.SetDurable(fake)
	if !s.Granted(turnCtx("telegram", "42"), "tool:exec", "shell") {
		t.Error("a grant given on another node was not honoured")
	}
}

// A replication failure must not lose an answer the user already gave.
// It degrades to the process-local behaviour it had before, which is
// worse than replicating and much better than re-asking mid-task.
func TestAFailedReplicationStillGrantsLocally(t *testing.T) {
	t.Parallel()
	fake := newFakeGrants()
	fake.err = errors.New("not the leader")
	s := NewSessionApprovals()
	s.SetDurable(fake)

	ctx := turnCtx("telegram", "42")
	if !s.Grant(ctx, "tool:exec", "shell") {
		t.Fatal("a replication failure lost the grant entirely")
	}
	if !s.Granted(ctx, "tool:exec", "shell") {
		t.Error("the local fallback was not recorded")
	}
	// And the error is retrievable for a caller that wants to say
	// something about it.
	if err := s.DurableGrantErr(ctx, "tool:exec", "shell"); err == nil {
		t.Error("DurableGrantErr swallowed the replication failure")
	}
}

// The local map answers first, so an ordinary same-node grant does not
// pay a store read on every policy check.
func TestALocalGrantDoesNotConsultTheStore(t *testing.T) {
	t.Parallel()
	fake := newFakeGrants()
	s := NewSessionApprovals()
	s.SetDurable(fake)
	ctx := turnCtx("telegram", "42")
	s.Grant(ctx, "tool:exec", "shell")

	before := fake.calls
	for range 5 {
		if !s.Granted(ctx, "tool:exec", "shell") {
			t.Fatal("the grant stopped being honoured")
		}
	}
	if fake.calls != before {
		t.Errorf("Granted wrote to the store %d times", fake.calls-before)
	}
}

// Without a durable store it is the process-local map it always was —
// the behaviour a node with no raft gets, rather than a silently
// missing feature.
func TestWithoutADurableStoreItIsStillProcessLocal(t *testing.T) {
	t.Parallel()
	s := NewSessionApprovals()
	ctx := turnCtx("telegram", "42")
	if !s.Grant(ctx, "tool:exec", "shell") {
		t.Fatal("the grant was not recorded")
	}
	if !s.Granted(ctx, "tool:exec", "shell") {
		t.Error("the process-local grant was not honoured")
	}
	if s.DurableGrantErr(ctx, "tool:exec", "shell") != nil {
		t.Error("DurableGrantErr errored with no store wired")
	}
}

// A turn with no conversation identity has nothing to scope a grant
// to, and a key derived from nothing matches everything.
func TestAnAnonymousTurnCannotGrant(t *testing.T) {
	t.Parallel()
	fake := newFakeGrants()
	s := NewSessionApprovals()
	s.SetDurable(fake)

	if s.Grant(context.Background(), "tool:exec", "shell") {
		t.Error("an anonymous turn recorded a grant")
	}
	if fake.calls != 0 {
		t.Error("an anonymous turn reached the durable store")
	}
	if s.Granted(context.Background(), "tool:exec", "shell") {
		t.Error("an anonymous turn was granted")
	}
}

// The durable lookup is scoped the same way the local one is.
func TestTheDurableLookupIsPerConversation(t *testing.T) {
	t.Parallel()
	fake := newFakeGrants()
	fake.granted["telegram:42|tool:exec|shell"] = "user:alice"
	s := NewSessionApprovals()
	s.SetDurable(fake)

	if s.Granted(turnCtx("telegram", "99"), "tool:exec", "shell") {
		t.Error("a durable grant reached another conversation")
	}
	if s.Granted(turnCtx("rest", "42"), "tool:exec", "shell") {
		t.Error("a durable grant reached the same id on another channel")
	}
}

// A nil store grants nothing, so the zero value is the safe one.
func TestANilApprovalStoreGrantsNothing(t *testing.T) {
	t.Parallel()
	var s *SessionApprovals
	if s.Granted(turnCtx("telegram", "42"), "tool:exec", "shell") {
		t.Error("a nil store granted")
	}
}
