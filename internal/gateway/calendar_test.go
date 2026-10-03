package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/google/calendar"
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

func TestCalendarButtonsAreOwnedPrivateSingleUse(t *testing.T) {
	for _, scenario := range []string{"owner", "other-user", "group", "policy-revoked", "generic-grant", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			registry := NewPromptRegistry()
			h, _, _ := reviewTelegram(t, registry)
			calls := 0
			h.cfg.Calendar = func(_ context.Context, c *types.Claims, r CalendarRequest) (any, error) {
				calls++
				if c.UserID != "tg-1" || r.ID != "exact-change" || r.Operation != "settings_apply" {
					t.Fatal("wrong target")
				}
				return calendar.SettingsResult{}, nil
			}
			raw, _ := json.Marshal(calendarUI{Action: "apply", Request: CalendarRequest{Operation: "settings_apply", ID: "exact-change"}})
			p, err := registry.Create(NewPrompt{Action: "calendar:ui", Resource: string(raw), Channel: "telegram", ChannelID: "1", RaisedFor: "tg-1", TTL: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			q := &tgCallbackQuery{ID: "tap", Data: "cal:do:" + p.ID, From: &tgUser{ID: 1}, Message: &tgMessage{Chat: tgChat{ID: 1, Type: "private"}}}
			switch scenario {
			case "other-user":
				q.From.ID = 2
			case "group":
				q.Message.Chat.Type = "supergroup"
			case "policy-revoked":
				h.cfg.CommandAuthorizer = fakeAuthz{allow: false}
			case "generic-grant":
				q.Data = "prompt:approve-always:" + p.ID
			case "expired":
				registry.mu.Lock()
				registry.prompts[p.ID].ExpiresAt = time.Now().Add(-time.Minute)
				registry.mu.Unlock()
			}
			h.handleCallbackQuery(t.Context(), q)
			h.handleCallbackQuery(t.Context(), q)
			want := 0
			if scenario == "owner" {
				want = 1
			}
			if calls != want {
				t.Fatalf("calls=%d want=%d", calls, want)
			}
		})
	}
}
func TestCalendarSelectionReviewsAllSelectedGrants(t *testing.T) {
	registry := NewPromptRegistry()
	h, _, _ := reviewTelegram(t, registry)
	activations := 0
	h.cfg.Calendar = func(_ context.Context, _ *types.Claims, r CalendarRequest) (any, error) {
		switch r.Operation {
		case "pending":
			return calendar.PendingView{ID: "flow", Email: "user@example.test", Write: true, Calendars: []calendar.CalendarView{{ID: "a", Name: "Personal", AccessRole: "owner"}, {ID: "b", Name: "Work", AccessRole: "reader"}}}, nil
		case "activate":
			activations++
			if len(r.Calendars) != 2 || !r.Calendars["a"].Write || r.Calendars["b"].Write || r.Email != "user@example.test" {
				t.Fatalf("wrong activation %+v", r)
			}
			return calendar.ConnectionView{}, nil
		}
		t.Fatal(r.Operation)
		return nil, nil
	}
	selection := CalendarRequest{ID: "flow", Calendars: map[string]calendar.Permission{"a": {Read: true, Write: true}, "b": {Read: true}}}
	if err := h.calendarUI(t.Context(), 1, &types.Claims{UserID: "tg-1"}, calendarUI{Action: "review", Request: selection}); err != nil {
		t.Fatal(err)
	}
	if activations != 0 {
		t.Fatal("review activated without human")
	}
	var approve *Prompt
	registry.mu.Lock()
	for _, p := range registry.prompts {
		var target calendarUI
		_ = json.Unmarshal([]byte(p.Resource), &target)
		if target.Action == "activate" {
			approve = p
		}
	}
	registry.mu.Unlock()
	if approve == nil {
		t.Fatal("no exact selection prompt")
	}
	q := &tgCallbackQuery{ID: "tap", Data: "cal:do:" + approve.ID, From: &tgUser{ID: 1}, Message: &tgMessage{Chat: tgChat{ID: 1, Type: "private"}}}
	h.handleCallbackQuery(t.Context(), q)
	if activations != 1 {
		t.Fatal("selection not activated")
	}
}
