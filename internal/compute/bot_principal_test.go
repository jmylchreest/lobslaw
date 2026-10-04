package compute

import (
	"testing"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Memory ownership, policy subjects and audit all decide against the
// turn's Principal, so getting it wrong for a bot is not a cosmetic
// bug — it is the isolation guarantee failing silently. A bot would
// own nothing it wrote and match no rule written about it, and both
// failures look like "the bot just isn't working".
//
// The specific hazard: Resolver.Resolve wraps whatever it is handed in
// the USER kind, so putting "bot:devops" through it yields
// "user:bot:devops". The same double-prefix trap Principal.ID
// documents, reached from the other direction.

func TestBotTurnGetsABotPrincipalNotADoublePrefixedUser(t *testing.T) {
	t.Parallel()
	agent, err := NewAgent(AgentConfig{Provider: NewMockProvider(MockResponse{Content: "ok"})})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	got := agent.TurnIdentityFor(ProcessMessageRequest{
		BotID:  "devops",
		Claims: &types.Claims{UserID: "bot:devops", Scope: "bot:devops"},
	})

	if want := identity.Bot("devops"); got.Principal != want {
		t.Errorf("Principal = %q, want %q", got.Principal, want)
	}
	if !got.Principal.IsBot() {
		t.Error("the principal does not read as a bot")
	}
	if !got.IsBot() {
		t.Error("Identity.IsBot() = false on a bot's turn")
	}
	if got.BotID != "devops" {
		t.Errorf("BotID = %q, want %q", got.BotID, "devops")
	}
}

// A routine alice scheduled is worked by the devops bot and attributed
// to alice. So a bot turn carrying somebody else's claims keeps their
// principal — the bot is doing the work, not owning it.
func TestBotTurnRunForAPersonKeepsTheirPrincipal(t *testing.T) {
	t.Parallel()
	agent, err := NewAgent(AgentConfig{Provider: NewMockProvider(MockResponse{Content: "ok"})})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	got := agent.TurnIdentityFor(ProcessMessageRequest{
		BotID:  "devops",
		Claims: &types.Claims{UserID: "alice", Scope: "scheduler"},
	})

	if want := identity.User("alice"); got.Principal != want {
		t.Errorf("Principal = %q, want %q — the work is attributed to the person who asked", got.Principal, want)
	}
	if got.BotID != "devops" {
		t.Errorf("BotID = %q, want the bot that ran it", got.BotID)
	}
}

func TestTurnWithNoBotIsUnchanged(t *testing.T) {
	t.Parallel()
	agent, err := NewAgent(AgentConfig{Provider: NewMockProvider(MockResponse{Content: "ok"})})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	got := agent.TurnIdentityFor(ProcessMessageRequest{
		Claims: &types.Claims{UserID: "alice", Scope: "default"},
	})
	if want := identity.User("alice"); got.Principal != want {
		t.Errorf("Principal = %q, want %q", got.Principal, want)
	}
	if got.IsBot() {
		t.Error("a turn with no bot reported IsBot()")
	}
}

// A bot and a person that share an id own different records. If the
// two principals ever collided, one would read the other's memories.
func TestBotAndPersonWithTheSameNameDoNotCollide(t *testing.T) {
	t.Parallel()
	agent, err := NewAgent(AgentConfig{Provider: NewMockProvider(MockResponse{Content: "ok"})})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	bot := agent.TurnIdentityFor(ProcessMessageRequest{
		BotID:  "engineering",
		Claims: &types.Claims{UserID: "bot:engineering"},
	})
	person := agent.TurnIdentityFor(ProcessMessageRequest{
		Claims: &types.Claims{UserID: "engineering"},
	})
	if bot.Principal == person.Principal {
		t.Errorf("bot and person collided on principal %q", bot.Principal)
	}
}
