package identity

import "testing"

// A bot is a principal so that per-bot isolation falls out of the
// machinery that already decides against principals — memory
// ownership, policy subjects, scheduled-task owners. The tests below
// pin the properties the rest of that machinery relies on.

func TestBotIsItsOwnKind(t *testing.T) {
	t.Parallel()
	p := Bot("engineering")
	if got, want := p.String(), "bot:engineering"; got != want {
		t.Errorf("Bot(%q) = %q, want %q", "engineering", got, want)
	}
	if got, want := p.ID(), "engineering"; got != want {
		t.Errorf("ID() = %q, want %q", got, want)
	}
	if !p.IsBot() {
		t.Error("IsBot() = false for a bot principal")
	}
}

// Two kinds must never collide on a bare id. A person called
// "engineering" and the engineering bot own different records, and a
// reader that cannot tell them apart is a reader that leaks one into
// the other.
func TestBotAndUserWithTheSameIDAreDifferentPrincipals(t *testing.T) {
	t.Parallel()
	if Bot("engineering") == User("engineering") {
		t.Error("bot and user principals collided on a shared id")
	}
	if User("engineering").IsBot() {
		t.Error("IsBot() = true for a person")
	}
	if Chat("telegram", "-100").IsBot() {
		t.Error("IsBot() = true for a conversation")
	}
}

// Same contract as User: absence is the empty Principal, not "bot:",
// so callers test for absence without knowing the encoding.
func TestBotWithNoIDIsTheZeroPrincipal(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"", "   "} {
		if p := Bot(id); !p.IsZero() {
			t.Errorf("Bot(%q) = %q, want the zero Principal", id, p)
		}
	}
}

// The alias map exists to translate ids arriving FROM a channel. If a
// channel could steer resolution into the bot kind, an inbound message
// could claim to be one of the assistant's own agents — and bots are
// the principals that policy rules trust. Resolve only ever mints
// users.
func TestResolveNeverMintsABotPrincipal(t *testing.T) {
	t.Parallel()
	r := NewResolver(map[string]string{
		"tg-@mallory": "engineering",
		"bot:sneaky":  "bot:sneaky",
	})
	for _, in := range []string{"tg-@mallory", "bot:sneaky", "bot:engineering"} {
		if got := r.Resolve(in); got.IsBot() {
			t.Errorf("Resolve(%q) = %q, which is a bot principal", in, got)
		}
	}
}

func TestUniqueOperatorRequiresExactlyOne(t *testing.T) {
	t.Parallel()
	if got := UniqueOperator(nil); !got.IsZero() {
		t.Errorf("UniqueOperator(nil) = %q, want empty", got)
	}
	if got := UniqueOperator([]string{}); !got.IsZero() {
		t.Errorf("UniqueOperator(empty) = %q, want empty", got)
	}
	if got := UniqueOperator([]string{"alice", "bob"}); !got.IsZero() {
		t.Errorf("UniqueOperator(two) = %q, want empty — ambiguous must not pick a winner", got)
	}
	if got, want := UniqueOperator([]string{"alice"}), User("alice"); got != want {
		t.Errorf("UniqueOperator([alice]) = %q, want %q", got, want)
	}
}
