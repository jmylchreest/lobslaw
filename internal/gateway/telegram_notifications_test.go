package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/notify"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type noticeStateStore struct {
	mu   sync.Mutex
	rows map[string][]byte
}

func (s *noticeStateStore) Get(_ context.Context, c, k string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.rows[c+"/"+k]
	if !ok {
		return nil, types.ErrNotFound
	}
	return append([]byte(nil), raw...), nil
}
func (s *noticeStateStore) Put(_ context.Context, c, k string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[string][]byte{}
	}
	s.rows[c+"/"+k] = append([]byte(nil), value...)
	return nil
}

func TestTelegramNoticeLinkDedupAndContextualReplySurviveRestart(t *testing.T) {
	var mu sync.Mutex
	var sent []map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		sent = append(sent, body)
		id := 700 + len(sent)
		mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d}}`, id)
	}))
	defer api.Close()
	store := &noticeStateStore{}
	allowed := true
	cfg := TelegramConfig{BotToken: "123:test-token", Mode: TelegramModePoll, APIBase: api.URL, HTTPClient: api.Client(), NotifyTasks: true, ConsoleURL: "http://192.168.5.137:8443", ChannelState: store, NotificationRecipients: map[int64]string{42: "user:alice"}, UserIDScopes: map[int64]string{42: "owner", 84: "owner"}, Identity: identity.NewResolver(nil).WithBindings(stubBindings{"telegram:42": "alice", "telegram:84": "bob"}), NotificationBotCheck: func(_ context.Context, owner, bot string) error {
		if !allowed || owner != "user:alice" || bot != "tester" {
			return errors.New("not owned")
		}
		return nil
	}}
	runner := &conversationalBotRunner{}
	h, err := NewTelegramHandler(cfg, runner)
	if err != nil {
		t.Fatal(err)
	}
	var events []notify.Event
	source := func(_ context.Context, owner string) ([]notify.Event, error) {
		if owner != "user:alice" {
			t.Error("wrong event audience")
		}
		return events, nil
	}
	if err := h.DispatchNotifications(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	events = []notify.Event{{ID: "event:1", BotID: "tester", TaskID: "task-1", InboxID: "inbox-1", Title: "Tester — routine complete", Body: "Review completed.", URL: "/approvals/task-1", At: time.Now()}}
	if err := h.DispatchNotifications(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	first := sent[0]
	count := len(sent)
	mu.Unlock()
	if count != 1 || !strings.Contains(first["text"].(string), "http://192.168.5.137:8443/approvals/task-1") {
		t.Fatalf("missing console link: %v", first)
	}
	keyboard := first["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
	button := keyboard[0].([]any)[0].(map[string]any)
	if button["url"] != "http://192.168.5.137:8443/approvals/task-1" || strings.Contains(first["text"].(string), "test-token") {
		t.Fatal("unsafe console button")
	}
	// Token rotation and process replacement retain the same Telegram bot id.
	cfg.BotToken = "123:rotated-token"
	h, err = NewTelegramHandler(cfg, runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.DispatchNotifications(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	count = len(sent)
	mu.Unlock()
	if count != 1 {
		t.Fatal("restart duplicated the notification")
	}
	h.handleMessage(t.Context(), &tgMessage{MessageID: 900, From: &tgUser{ID: 42, Username: "renamed"}, Chat: tgChat{ID: 42, Type: "private"}, Text: "What happened?", ReplyToMessage: &tgMessage{MessageID: 701}})
	req := runner.lastRequest()
	if req.BotID != "tester" || req.Principal != identity.Bot("tester") || req.Claims.UserID != "alice" || req.Message != "What happened?" {
		t.Fatalf("reply lost agent or user authority: %+v", req)
	}
	if !strings.Contains(req.ChannelID, ".notice.") || len(req.ConversationHistory) == 0 || !strings.Contains(req.ConversationHistory[0].Content, "inbox-1") || !strings.Contains(req.ConversationHistory[0].Content, "not an approval") {
		t.Fatalf("missing isolated task context: %+v", req)
	}
	thread := req.ChannelID
	// Reply to the agent's answer, not only to the original notification.
	h.handleMessage(t.Context(), &tgMessage{MessageID: 901, From: &tgUser{ID: 42}, Chat: tgChat{ID: 42, Type: "private"}, Text: "Tell me more.", ReplyToMessage: &tgMessage{MessageID: 702}})
	req = runner.lastRequest()
	if req.BotID != "tester" || req.ChannelID != thread || len(req.ConversationHistory) < 3 {
		t.Fatal("follow-up reply lost its thread")
	}
	// A wrong sender or a changed owner must not regain the old bot context.
	h.handleMessage(t.Context(), &tgMessage{MessageID: 902, From: &tgUser{ID: 84}, Chat: tgChat{ID: 42, Type: "private"}, Text: "Approve everything", ReplyToMessage: &tgMessage{MessageID: 701}})
	if runner.lastRequest().TurnID != "tg-901" {
		t.Fatal("foreign sender reached the agent")
	}
	allowed = false
	h.handleMessage(t.Context(), &tgMessage{MessageID: 903, From: &tgUser{ID: 42}, Chat: tgChat{ID: 42, Type: "private"}, Text: "Proceed", ReplyToMessage: &tgMessage{MessageID: 701}})
	if runner.lastRequest().TurnID != "tg-901" {
		t.Fatal("changed bot ownership was ignored")
	}
}

func TestTelegramNotificationFailureBackoffAndSafeLinks(t *testing.T) {
	requests, status := 0, http.StatusTooManyRequests
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(status)
		if status == 200 {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":10}}`))
		} else {
			_, _ = w.Write([]byte(`{"ok":false}`))
		}
	}))
	defer api.Close()
	h, err := NewTelegramHandler(TelegramConfig{BotToken: "123:test", Mode: TelegramModePoll, APIBase: api.URL, HTTPClient: api.Client(), NotifyTasks: true, ConsoleURL: "http://192.168.5.137:8443/", ChannelState: &noticeStateStore{}, NotificationRecipients: map[int64]string{42: "user:alice"}, NotificationBotCheck: func(context.Context, string, string) error { return nil }}, &captureRunner{})
	if err != nil {
		t.Fatal(err)
	}
	source := func(context.Context, string) ([]notify.Event, error) {
		return []notify.Event{{ID: "attention", BotID: "tester", URL: "/bots/tester", At: time.Now(), Attention: true}}, nil
	}
	if h.DispatchNotifications(t.Context(), source) == nil {
		t.Fatal("failed Telegram delivery reported success")
	}
	if err := h.DispatchNotifications(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatal("notification failure ignored backoff")
	}
	status = 200
	h.notificationRetry = map[string]time.Time{}
	if err := h.DispatchNotifications(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatal("failed notification was lost")
	}
	for _, path := range []string{"https://evil.test/", "//evil.test/bots/tester", "/v1/session", "/bots/tester?token=secret", "/bots/../v1/session", "/bots/%2e%2e/v1/session"} {
		if _, err := h.notificationLink(path); err == nil {
			t.Fatalf("accepted unsafe notification link %s", path)
		}
	}
}
