package calendar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/turn"
)

func TestOAuthCallbackBindingReplayAndHumanActivation(t *testing.T) {
	t.Parallel()
	exchanges := 0
	s, ctx := fixture(t, true, func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case tokenEndpoint:
			exchanges++
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("code_verifier") == "" {
				t.Fatal("missing PKCE verifier")
			}
			return response(`{"access_token":"pending-access","refresh_token":"pending-refresh","expires_in":3600,"token_type":"Bearer","scope":"openid email ` + scopeCalendarList + ` ` + scopeEventsWrite + `"}`), nil
		case userInfoEndpoint:
			return response(`{"sub":"stable-sub","email":"alice@example.test","email_verified":true}`), nil
		default:
			if strings.Contains(r.URL.Path, "calendarList") {
				return response(`{"items":[{"id":"cal","accessRole":"owner"}]}`), nil
			}
			t.Fatalf("unexpected URL %s", r.URL)
			return nil, nil
		}
	})
	intent, err := s.Begin(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	start := httptest.NewRecorder()
	s.BrowserHandler().ServeHTTP(start, httptest.NewRequest(http.MethodGet, intent.URL, nil))
	if start.Code != http.StatusSeeOther {
		t.Fatalf("start=%d", start.Code)
	}
	location, _ := url.Parse(start.Header().Get("Location"))
	if location.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("missing PKCE challenge")
	}
	callback := s.cfg.RedirectURL + "?state=" + intent.ID + "&code=test-code"
	noCookie := httptest.NewRecorder()
	s.BrowserHandler().ServeHTTP(noCookie, httptest.NewRequest(http.MethodGet, callback, nil))
	if noCookie.Code == http.StatusOK || exchanges != 0 {
		t.Fatal("unbound callback exchanged code")
	}
	req := httptest.NewRequest(http.MethodGet, callback, nil)
	req.AddCookie(start.Result().Cookies()[0])
	result := httptest.NewRecorder()
	s.BrowserHandler().ServeHTTP(result, req)
	if result.Code != http.StatusOK {
		t.Fatalf("callback=%d %s", result.Code, result.Body)
	}
	replay := httptest.NewRecorder()
	s.BrowserHandler().ServeHTTP(replay, req)
	if replay.Code == http.StatusOK || exchanges != 1 {
		t.Fatal("replayed authorization code")
	}
	other := turn.WithIdentity(context.Background(), turn.Identity{Principal: "bob"})
	if _, err := s.Pending(other, intent.ID); err == nil {
		t.Fatal("other user reviewed account")
	}
	if _, err := s.Activate(ctx, intent.ID, "wrong@example.test", map[string]Permission{"cal": {Read: true}}); err == nil {
		t.Fatal("wrong account activated")
	}
	view, err := s.Activate(ctx, intent.ID, "alice@example.test", map[string]Permission{"cal": {Read: true}})
	if err != nil {
		t.Fatal(err)
	}
	if view.Calendars["cal"].Write {
		t.Fatal("Google write consent enabled local write")
	}
	if _, err := s.Activate(ctx, intent.ID, "alice@example.test", map[string]Permission{"cal": {Read: true}}); err == nil {
		t.Fatal("duplicate activation")
	}
}

func TestCalendarBoundariesAndMutationValidation(t *testing.T) {
	t.Parallel()
	s, ctx := fixture(t, true, func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid operation reached Google")
		return nil, nil
	})
	q := Query{Connection: "connection", Calendar: "other", Start: "2026-10-03T00:00:00Z", End: "2026-10-04T00:00:00Z"}
	if _, err := s.Events(ctx, q); err == nil {
		t.Fatal("unselected calendar")
	}
	q.Calendar = "cal"
	ctx = turn.WithIdentity(ctx, turn.Identity{Principal: "bob", TurnID: "turn"})
	if _, err := s.Events(ctx, q); err == nil {
		t.Fatal("cross-owner read")
	}
	for _, operation := range []string{"delete", "cancel", "move"} {
		if _, err := s.Prepare(ctx, operation, Mutation{}); err == nil {
			t.Fatalf("unsupported %s", operation)
		}
	}
}
