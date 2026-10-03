package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/auth"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestCalendarManagementRequiresAuthenticatedHuman(t *testing.T) {
	validator, err := auth.NewValidator(auth.Config{AllowHS256: true, HS256Secret: restTestSecret})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	api := func(_ context.Context, c *types.Claims, r CalendarRequest) (any, error) {
		calls++
		if c.UserID != "test-user" || r.Operation != "list" {
			t.Fatal("wrong request identity")
		}
		return []string{}, nil
	}
	server := NewServer(RESTConfig{JWTValidator: validator, Calendar: api}, nil)
	for _, tc := range []struct {
		body   string
		auth   bool
		status int
	}{{`{"operation":"list"}`, false, 401}, {`{"operation":"list","owner":"bob"}`, true, 400}, {`{"operation":"list"}{}`, true, 400}, {`{"operation":"list"}`, true, 200}} {
		req := httptest.NewRequest(http.MethodPost, "/v1/calendar", strings.NewReader(tc.body))
		if tc.auth {
			req.Header.Set("Authorization", "Bearer "+mintValidJWT(t, "owner"))
		}
		w := httptest.NewRecorder()
		server.handleCalendar(w, req)
		if w.Code != tc.status {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}
func TestCalendarCommandNeverExposesAccountInSharedChat(t *testing.T) {
	calls := 0
	h := &TelegramHandler{cfg: TelegramConfig{Calendar: func(context.Context, *types.Claims, CalendarRequest) (any, error) { calls++; return "account", nil }}, commands: NewCommandSet(fakeAuthz{allow: true}, discardLogger())}
	h.registerCalendarCommand()
	req := CommandRequest{Name: "calendar", Shared: true, Claims: &types.Claims{UserID: "alice"}}
	if result := h.commands.Dispatch(t.Context(), req); !strings.Contains(result, "direct message") {
		t.Fatal(result)
	}
	if calls != 0 {
		t.Fatal("shared account access")
	}
	req.Shared = false
	_ = h.commands.Dispatch(t.Context(), req)
	if calls != 1 {
		t.Fatal("private command did not reach management")
	}
}
