package gateway

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// turnIdentitySpy reads the identity the agent attached to the turn
// context. Hooks are the cheapest observation point that sits inside
// the agent loop — a mock provider only sees the prompt, and the
// prompt is exactly where identity is deliberately absent.
type turnIdentitySpy struct {
	mu   sync.Mutex
	seen turn.Identity
	ok   bool
}

func (s *turnIdentitySpy) Dispatch(ctx context.Context, _ types.HookEvent, _ map[string]any) (*compute.HookResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen, s.ok = turn.IdentityFrom(ctx)
	return nil, nil
}

func (s *turnIdentitySpy) identity(t *testing.T) turn.Identity {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ok {
		t.Fatal("no turn identity reached the agent loop")
	}
	return s.seen
}

// TestTelegramCarriesConfigDeclaredRoles is the non-JWT half of the
// operator role. Telegram has no token to put a `roles` claim in, so
// without the operator's declaration reaching Claims here, no rule
// written against subject = "role:operator" can ever match a turn
// from this channel.
func TestTelegramCarriesConfigDeclaredRoles(t *testing.T) {
	t.Parallel()
	spy := &turnIdentitySpy{}
	agent, err := compute.NewAgent(compute.AgentConfig{
		Provider: compute.NewMockProvider(compute.MockResponse{Content: "ack"}),
		Hooks:    spy,
	})
	if err != nil {
		t.Fatal(err)
	}

	var asked []string
	h := newTGHarness(t, agent, TelegramConfig{
		UserIDScopes: map[int64]string{12345: "default"},
		Roles: func(userID string) []string {
			asked = append(asked, userID)
			if userID == "tg-@alice" {
				return []string{"operator"}
			}
			return nil
		},
	})

	update := `{
		"update_id": 1,
		"message": {
			"message_id": 1,
			"from": {"id": 12345, "username": "alice"},
			"chat": {"id": 111, "type": "private"},
			"text": "hello"
		}
	}`
	if rec := postUpdate(t, h.handler, tgTestSecret, update); rec.Code != http.StatusOK {
		t.Fatalf("update rejected: %d", rec.Code)
	}

	turn := spy.identity(t)
	if !turn.Claims().HasRole("operator") {
		t.Errorf("turn roles = %v; want the operator role from [[user]]", turn.Roles)
	}
	if len(asked) == 0 || asked[0] != "tg-@alice" {
		t.Errorf("role lookup asked about %v; want the channel user id", asked)
	}
}

// TestTelegramWithoutRoleResolverIsRoleless keeps the absence of the
// wiring meaning "no roles" rather than panicking or inventing one.
func TestTelegramWithoutRoleResolverIsRoleless(t *testing.T) {
	t.Parallel()
	spy := &turnIdentitySpy{}
	agent, err := compute.NewAgent(compute.AgentConfig{
		Provider: compute.NewMockProvider(compute.MockResponse{Content: "ack"}),
		Hooks:    spy,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := newTGHarness(t, agent, TelegramConfig{UnknownUserScope: "public"})

	update := `{
		"update_id": 1,
		"message": {
			"message_id": 1,
			"from": {"id": 999, "username": "carol"},
			"chat": {"id": 222, "type": "private"},
			"text": "hello"
		}
	}`
	if rec := postUpdate(t, h.handler, tgTestSecret, update); rec.Code != http.StatusOK {
		t.Fatalf("update rejected: %d", rec.Code)
	}
	if turn := spy.identity(t); len(turn.Roles) != 0 {
		t.Errorf("roles = %v; want none", turn.Roles)
	}
}
