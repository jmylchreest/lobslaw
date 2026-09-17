package gateway

import (
	"context"
	"errors"
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
	switch id {
	case "gone":
		// A team that was deleted while its bots still point at it.
		return nil, errors.New("groups: not found")
	case "sam-side":
		return &lobslawv1.GroupRecord{Id: id, Name: "Sam Side", Owner: "user:sam"}, nil
	default:
		return &lobslawv1.GroupRecord{Id: id, Name: "James Core", Owner: "user:james"}, nil
	}
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
// authzServer builds a console with two people and three bots — one in
// James's team, one in Sam's, one whose team has been deleted — plus a
// helper that signs in and issues a request the way the console does.
func authzServer(t *testing.T) (*Server, func(*testing.T, string, string, string, string) *httptest.ResponseRecorder) {
	t.Helper()
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
		Bots: newFakeBots(
			&lobslawv1.BotRecord{Id: "coordinator", IsCoordinator: true, Enabled: true, GroupId: "james-core"},
			&lobslawv1.BotRecord{Id: "drifter", Enabled: true, GroupId: "sam-side"},
			&lobslawv1.BotRecord{Id: "orphan", Enabled: true, GroupId: "gone"},
		),
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
	return s, as
}

func TestChangingABotRequiresOwningItsTeam(t *testing.T) {
	t.Parallel()

	_, as := authzServer(t)

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

// The destination of a write matters as much as its source.
//
// The first round gated PATCH and DELETE on the bot's CURRENT team and
// stopped there, which left two ways past it: create a bot directly
// inside somebody else's team, or move one into it. Both are the
// threat the guard exists to close, one route over.
func TestWritingIntoATeamRequiresOwningTheDestination(t *testing.T) {
	t.Parallel()

	_, as := authzServer(t)

	t.Run("cannot create a bot in another person's team", func(t *testing.T) {
		rec := as(t, "sam-token", http.MethodPost, "/v1/bots",
			`{"id":"mole","display_name":"Mole","group_id":"james-core","tools":["shell_command"]}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("got %d, want 403 — an enabled bot with tools of its own "+
				"choosing was created inside somebody else's team", rec.Code)
		}
	})

	t.Run("can create a bot in their own team", func(t *testing.T) {
		rec := as(t, "sam-token", http.MethodPost, "/v1/bots",
			`{"id":"sams-bot","display_name":"Sams Bot","group_id":"sam-side"}`)
		if rec.Code != http.StatusCreated {
			t.Errorf("owner got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("cannot move a bot into another person's team", func(t *testing.T) {
		// Sam owns the bot's current team, so the source check passes —
		// which is exactly the case the destination check is for.
		rec := as(t, "sam-token", http.MethodPatch, "/v1/bots/drifter",
			`{"group_id":"james-core"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("got %d, want 403 — a bot was moved into a team the caller does not own", rec.Code)
		}
	})

	// Deleting a team used to make every bot left in it editable by
	// anyone signed in: the group lookup failed and mayModifyBot
	// returned true.
	t.Run("a bot whose team is gone is not open to everyone", func(t *testing.T) {
		rec := as(t, "sam-token", http.MethodPatch, "/v1/bots/orphan",
			`{"instructions":"mine now"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("got %d, want 403 — an unreadable team failed open", rec.Code)
		}
	})
}
