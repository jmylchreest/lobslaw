package identity

import (
	"context"
	"errors"
	"testing"
)

// Telegram's user id is the @username when the user has one, and a
// username is reassignable. Bound to that, a rename orphans somebody's
// history and grants, and whoever claims the freed handle inherits
// them. Resolving from the raw address instead lets an operator bind
// the numeric id, which never changes.

type fakeBindings struct {
	byAddress map[string]string
	err       error
}

func (f fakeBindings) PrincipalFor(_ context.Context, channel, address string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.byAddress[channel+":"+address], nil
}

// The case R2b is about. Alice is bound by her numeric id, so the
// handle she happens to be using is irrelevant.
func TestRenamingDoesNotOrphanAnIdentity(t *testing.T) {
	t.Parallel()
	r := NewResolver(nil).WithBindings(fakeBindings{
		byAddress: map[string]string{"telegram:123456789": "alice"},
	})

	// Before the rename, and after it: same principal.
	for _, handle := range []string{"tg-@alice", "tg-@alice_new", "tg-123456789"} {
		got, err := r.ResolveChannel(context.Background(), "telegram", "123456789", handle)
		if err != nil {
			t.Fatal(err)
		}
		if got != User("alice") {
			t.Errorf("handle %q resolved to %v, want user:alice", handle, got)
		}
	}
}

// The other half, and the one that fails open rather than closed:
// somebody claiming the freed handle must not inherit anything.
func TestClaimingAFreedHandleInheritsNothing(t *testing.T) {
	t.Parallel()
	r := NewResolver(nil).WithBindings(fakeBindings{
		byAddress: map[string]string{"telegram:123456789": "alice"},
	})

	// A different person, now using the handle Alice used to have.
	got, err := r.ResolveChannel(context.Background(), "telegram", "999999999", "tg-@alice")
	if err != nil {
		t.Fatal(err)
	}
	if got == User("alice") {
		t.Fatal("a stranger using Alice's old handle resolved to Alice")
	}
	if got != User("tg-@alice") {
		t.Errorf("got %v, want the unbound address to become its own principal", got)
	}
}

// A deployment that binds nothing must behave exactly as it did
// before. Requiring registration would mean a new person cannot talk
// to the bot until an operator edits a file.
func TestUnboundAddressKeepsTodaysBehaviour(t *testing.T) {
	t.Parallel()
	for name, r := range map[string]*Resolver{
		"no resolver at all": nil,
		"no bindings":        NewResolver(nil),
		"empty bindings":     NewResolver(nil).WithBindings(fakeBindings{}),
	} {
		got, err := r.ResolveChannel(context.Background(), "telegram", "555", "tg-@bob")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != User("tg-@bob") {
			t.Errorf("%s: got %v, want user:tg-@bob", name, got)
		}
	}
}

// The alias map still works for anything bindings do not cover, so
// existing configs keep resolving.
func TestAliasMapStillAppliesWhenNothingIsBound(t *testing.T) {
	t.Parallel()
	r := NewResolver(map[string]string{"tg-@bob": "bob"}).
		WithBindings(fakeBindings{byAddress: map[string]string{"telegram:1": "alice"}})

	got, err := r.ResolveChannel(context.Background(), "telegram", "2", "tg-@bob")
	if err != nil {
		t.Fatal(err)
	}
	if got != User("bob") {
		t.Errorf("got %v, want the alias map to still resolve bob", got)
	}
}

// A binding outranks the alias map: it names a stable address, where
// the alias map names a string the channel already derived.
func TestBindingBeatsTheAliasMap(t *testing.T) {
	t.Parallel()
	r := NewResolver(map[string]string{"tg-@alice": "old-alice"}).
		WithBindings(fakeBindings{byAddress: map[string]string{"telegram:1": "alice"}})

	got, err := r.ResolveChannel(context.Background(), "telegram", "1", "tg-@alice")
	if err != nil {
		t.Fatal(err)
	}
	if got != User("alice") {
		t.Errorf("got %v, want the binding to win", got)
	}
}

// A lookup outage must not reassign somebody's identity or lock them
// out of their own history. It reports the error AND a usable
// principal, so the caller can log one and use the other.
func TestLookupFailureFallsBackWithoutLosingTheError(t *testing.T) {
	t.Parallel()
	boom := errors.New("store unavailable")
	r := NewResolver(map[string]string{"tg-@alice": "alice"}).
		WithBindings(fakeBindings{err: boom})

	got, err := r.ResolveChannel(context.Background(), "telegram", "1", "tg-@alice")
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the lookup failure surfaced", err)
	}
	if got != User("alice") {
		t.Errorf("got %v; an outage locked the user out of their own history", got)
	}
}

// Claims.UserID is a bare id — policy subjects are written
// "user:alice" and the engine adds the kind itself, so passing a
// principal through would produce "user:user:alice" and match nothing.
// REST JWT subjects bind the same way Telegram numeric ids do: the
// operator's [[user.channels]] address, not a display name the client
// chose.
func TestRESTAddressResolvesToDeclaredUser(t *testing.T) {
	t.Parallel()
	r := NewResolver(nil).WithBindings(fakeBindings{
		byAddress: map[string]string{"rest:alice@idp": "alice"},
	})
	got, err := r.ResolveChannel(context.Background(), "rest", "alice@idp", "Alice The Operator")
	if err != nil {
		t.Fatal(err)
	}
	if got != User("alice") {
		t.Errorf("got %v, want user:alice", got)
	}
}

// Matching display names across channels is not linking. Resolution
// keys on the channel address (or a caller-supplied fallback id),
// never on the human-readable name.
func TestMatchingDisplayNamesAreNotALink(t *testing.T) {
	t.Parallel()
	r := NewResolver(nil)
	rest, err := r.ResolveChannel(context.Background(), "rest", "alice@idp", "alice@idp")
	if err != nil {
		t.Fatal(err)
	}
	tg, err := r.ResolveChannel(context.Background(), "telegram", "999", "tg-999")
	if err != nil {
		t.Fatal(err)
	}
	if rest == tg {
		t.Fatal("unbound rest and telegram addresses became one principal")
	}
	if r.Resolve("Alice") == rest || r.Resolve("Alice") == tg {
		t.Fatal("a display name resolved to a channel address")
	}
}

func TestIDStripsTheKind(t *testing.T) {
	t.Parallel()
	for p, want := range map[Principal]string{
		User("alice"):            "alice",
		Chat("telegram", "-100"): "telegram:-100",
		Principal("alice"):       "alice",
		Principal(""):            "",
	} {
		if got := p.ID(); got != want {
			t.Errorf("%q.ID() = %q, want %q", p, got, want)
		}
	}
}
