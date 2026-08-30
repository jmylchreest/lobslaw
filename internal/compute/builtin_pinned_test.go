package compute

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/turn"
)

// The cap is a hard error, which makes "you are full, consolidate" a
// livelock waiting to happen: a model can spend a whole turn failing
// to edit and never answer the person who is waiting. hermes shipped
// _MAX_CONSOLIDATION_FAILURES_PER_TURN = 3 after exactly that. A
// memory side effect must never cost somebody their reply.

type fakePinnedStore struct {
	mu      sync.Mutex
	entries map[string][]string
	// failWith, when set, makes every mutation fail — standing in for
	// a full block or a fragile match.
	failWith error
	calls    int
}

func newFakePinned() *fakePinnedStore {
	return &fakePinnedStore{entries: map[string][]string{}}
}

func (f *fakePinnedStore) key(kind, user string) string { return kind + ":" + user }

func (f *fakePinnedStore) Entries(kind, userID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.entries[f.key(kind, userID)], nil
}

func (f *fakePinnedStore) Add(_ context.Context, kind, userID, entry string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failWith != nil {
		return f.failWith
	}
	k := f.key(kind, userID)
	f.entries[k] = append(f.entries[k], entry)
	return nil
}

func (f *fakePinnedStore) Replace(_ context.Context, kind, userID, _, replacement string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failWith != nil {
		return f.failWith
	}
	k := f.key(kind, userID)
	if len(f.entries[k]) == 0 {
		return errors.New("nothing to replace")
	}
	f.entries[k][0] = replacement
	return nil
}

func (f *fakePinnedStore) Remove(_ context.Context, kind, userID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failWith != nil {
		return f.failWith
	}
	k := f.key(kind, userID)
	if len(f.entries[k]) == 0 {
		return errors.New("nothing to remove")
	}
	f.entries[k] = f.entries[k][1:]
	return nil
}

func (f *fakePinnedStore) Usage(kind, userID string) (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.entries[f.key(kind, userID)] {
		n += len(e) + 1
	}
	return n, 1000, nil
}

func pinnedCtx(turnID, userID string) context.Context {
	return turn.WithIdentity(context.Background(), turn.Identity{
		TurnID: turnID, UserID: userID,
	})
}

func pinnedHandlers(t *testing.T, store PinnedMemoryStore) (remember, forget BuiltinFunc) {
	t.Helper()
	b := NewBuiltins()
	if err := RegisterPinnedBuiltins(b, store); err != nil {
		t.Fatal(err)
	}
	r, ok := b.Get("pinned_remember")
	if !ok {
		t.Fatal("pinned_remember not registered")
	}
	f, ok := b.Get("pinned_forget")
	if !ok {
		t.Fatal("pinned_forget not registered")
	}
	return r, f
}

// The property the cap exists to protect: after N failures the tool
// stops and tells the model to answer, rather than inviting attempt
// N+1 until the turn's budget is gone.
func TestRepeatedFailuresGiveUpRatherThanLoop(t *testing.T) {
	t.Parallel()
	store := newFakePinned()
	store.failWith = errors.New("pinned memory: at capacity")
	remember, _ := pinnedHandlers(t, store)
	ctx := pinnedCtx("turn-1", "alice")

	// The first attempts fail as errors, which is right — the model
	// should get a chance to fix a typo or consolidate.
	for i := range maxPinnedFailuresPerTurn - 1 {
		if _, _, err := remember(ctx, map[string]string{"entry": "x"}); err == nil {
			t.Fatalf("attempt %d reported success against a failing store", i+1)
		}
	}

	// The one that trips the cap, and everything after it, must be a
	// terminal NON-error: an error invites another attempt, and the
	// point is that the model stops attempting.
	for i := range 5 {
		out, code, err := remember(ctx, map[string]string{"entry": "x"})
		if err != nil {
			t.Fatalf("call %d after the cap still returned an error, which invites a retry: %v", i, err)
		}
		if code != 0 {
			t.Errorf("call %d exit code = %d, want 0 — a non-zero code reads as retryable", i, code)
		}
		if !strings.Contains(string(out), "gave_up") {
			t.Errorf("call %d payload = %s", i, out)
		}
		if !strings.Contains(strings.ToLower(string(out)), "answer the user") {
			t.Errorf("call %d does not tell the model to answer: %s", i, out)
		}
	}

	// And it stops touching the store at all. A spent budget that
	// still round-trips is a livelock that merely costs less.
	store.mu.Lock()
	calls := store.calls
	store.mu.Unlock()
	if calls != maxPinnedFailuresPerTurn {
		t.Errorf("the store was called %d times, want %d — attempts continued past the cap",
			calls, maxPinnedFailuresPerTurn)
	}
}

// A different turn starts fresh. The cap is per-turn; carrying it
// across would leave a conversation unable to remember anything after
// one bad turn.
func TestFailureBudgetIsPerTurn(t *testing.T) {
	t.Parallel()
	store := newFakePinned()
	store.failWith = errors.New("boom")
	remember, _ := pinnedHandlers(t, store)

	for range maxPinnedFailuresPerTurn + 2 {
		_, _, _ = remember(pinnedCtx("turn-1", "alice"), map[string]string{"entry": "x"})
	}

	store.failWith = nil
	out, _, err := remember(pinnedCtx("turn-2", "alice"), map[string]string{"entry": "works now"})
	if err != nil {
		t.Fatalf("a new turn inherited the old turn's exhausted budget: %v", err)
	}
	if !strings.Contains(string(out), "stored") {
		t.Errorf("payload = %s", out)
	}
}

// A success clears the count, so a turn that recovers is not one
// mistake away from being cut off.
func TestSuccessResetsTheBudget(t *testing.T) {
	t.Parallel()
	store := newFakePinned()
	store.failWith = errors.New("boom")
	remember, _ := pinnedHandlers(t, store)
	ctx := pinnedCtx("turn-1", "alice")

	_, _, _ = remember(ctx, map[string]string{"entry": "x"})
	store.failWith = nil
	if _, _, err := remember(ctx, map[string]string{"entry": "ok"}); err != nil {
		t.Fatal(err)
	}

	store.failWith = errors.New("boom")
	// Back to a full allowance: the first failure after a success must
	// be an ordinary error, not the terminal result.
	if _, _, err := remember(ctx, map[string]string{"entry": "y"}); err == nil {
		t.Error("the budget was not reset by the successful write")
	}
}

// The user comes from the turn identity, never from a parameter. A
// user id the model can supply is one a prompt injection can supply,
// and writing into somebody else's always-on block would put attacker
// text in their system prompt on every future turn.
func TestUserComesFromTheTurnNotTheArguments(t *testing.T) {
	t.Parallel()
	store := newFakePinned()
	remember, _ := pinnedHandlers(t, store)

	if _, _, err := remember(pinnedCtx("t", "alice"), map[string]string{
		"entry": "a fact", "user_id": "bob", "user": "bob",
	}); err != nil {
		t.Fatal(err)
	}

	if got, _ := store.Entries("notes", "bob"); len(got) != 0 {
		t.Errorf("an argument redirected the write to bob: %v", got)
	}
	if got, _ := store.Entries("notes", "alice"); len(got) != 1 {
		t.Errorf("alice's block = %v", got)
	}
}

// An anonymous turn has nobody to remember for. Writing it somewhere
// would be writing it into a stranger's prompt.
func TestAnonymousTurnCannotWrite(t *testing.T) {
	t.Parallel()
	store := newFakePinned()
	remember, _ := pinnedHandlers(t, store)

	_, _, err := remember(context.Background(), map[string]string{"entry": "x"})
	if err == nil {
		t.Fatal("a turn with no identified user wrote to pinned memory")
	}
	if store.calls != 0 {
		t.Errorf("the store was called %d times for an anonymous turn", store.calls)
	}
}

func TestNotesIsTheDefaultKind(t *testing.T) {
	t.Parallel()
	store := newFakePinned()
	remember, _ := pinnedHandlers(t, store)

	if _, _, err := remember(pinnedCtx("t", "alice"), map[string]string{"entry": "a fact"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Entries("notes", "alice"); len(got) != 1 {
		t.Errorf("notes = %v; the default kind is not notes", got)
	}
	if got, _ := store.Entries("profile", "alice"); len(got) != 0 {
		t.Errorf("an unqualified fact landed in the profile: %v", got)
	}
}

func TestUnknownKindIsRejected(t *testing.T) {
	t.Parallel()
	store := newFakePinned()
	remember, _ := pinnedHandlers(t, store)

	if _, _, err := remember(pinnedCtx("t", "alice"), map[string]string{
		"entry": "x", "kind": "secrets",
	}); err == nil {
		t.Error("an unknown kind was accepted")
	}
}

// A successful write reports usage, so the model can see it is getting
// close without having to fail first.
func TestSuccessReportsUsage(t *testing.T) {
	t.Parallel()
	store := newFakePinned()
	remember, _ := pinnedHandlers(t, store)

	out, _, err := remember(pinnedCtx("t", "alice"), map[string]string{"entry": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"used_chars", "cap_chars"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("payload %s is missing %s", out, want)
		}
	}
}
