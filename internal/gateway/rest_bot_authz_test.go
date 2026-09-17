package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// ownedGroups reports one team owned by james.
type ownedGroups struct{}

func (ownedGroups) List(context.Context) ([]*lobslawv1.GroupRecord, error) { return nil, nil }
func (ownedGroups) Get(_ context.Context, id string) (*lobslawv1.GroupRecord, error) {
	return &lobslawv1.GroupRecord{Id: id, Name: "James Core", Owner: "user:james"}, nil
}
func (ownedGroups) Put(_ context.Context, rec *lobslawv1.GroupRecord, _ uint64) (*lobslawv1.GroupRecord, error) {
	return rec, nil
}
func (ownedGroups) Delete(context.Context, string) error { return nil }

// Changing a bot requires owning its team.
//
// Teams gained owners in this work and bots did not, which left the
// team check bypassable one route over: a bot Sam could not MOVE out
// of James's team, he could still re-brief, re-tool or delete —
// including the coordinator, which decides who answers on Telegram
// for that team. Owning the coordinator is owning the team in every
// way that matters.
func TestChangingABotRequiresOwningItsTeam(t *testing.T) {
	t.Parallel()

	key, err := DeriveConsoleKey([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("DeriveConsoleKey: %v", err)
	}
	s := NewServer(RESTConfig{
		ConsoleKey:  key,
		RequireAuth: true,
		ConsoleUsers: []ConsoleUser{
			{ID: "james", Token: "james-token"},
			{ID: "sam", Token: "sam-token"},
		},
		Bots: newFakeBots(&lobslawv1.BotRecord{
			Id: "coordinator", IsCoordinator: true, Enabled: true, GroupId: "james-core",
		}),
		Groups:       ownedGroups{},
		DefaultScope: "owner",
	}, nil)

	as := func(t *testing.T, token, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		// Sign in the way the console does, then reuse the cookie.
		login := httptest.NewRecorder()
		s.handleLogin(login, httptest.NewRequest(http.MethodPost, "/v1/auth/login",
			strings.NewReader(`{"token":"`+token+`"}`)))
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		for _, c := range login.Result().Cookies() {
			req.AddCookie(c)
		}
		s.handleBots(rec, req)
		return rec
	}

	t.Run("the owner may re-brief a bot in their team", func(t *testing.T) {
		rec := as(t, "james-token", http.MethodPatch, "/v1/bots/coordinator",
			`{"instructions":"New brief."}`)
		if rec.Code != http.StatusOK {
			t.Errorf("owner got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("somebody else may not re-brief it", func(t *testing.T) {
		rec := as(t, "sam-token", http.MethodPatch, "/v1/bots/coordinator",
			`{"instructions":"Actually, report to me."}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("got %d, want 403 — a bot in someone else's team was rewritten", rec.Code)
		}
	})

	t.Run("somebody else may not delete it", func(t *testing.T) {
		rec := as(t, "sam-token", http.MethodDelete, "/v1/bots/coordinator", "")
		if rec.Code != http.StatusForbidden {
			t.Errorf("got %d, want 403 — a bot in someone else's team was deleted", rec.Code)
		}
	})
}
