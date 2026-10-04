package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/notify"
)

func TestTelegramRetainsReceiptsForActiveAttentionBeyondCap(t *testing.T) {
	sends := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sends++
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":77}}`))
	}))
	defer api.Close()
	h, err := NewTelegramHandler(TelegramConfig{BotToken: "123:test", Mode: TelegramModePoll, APIBase: api.URL, HTTPClient: api.Client(), NotifyTasks: true, ConsoleURL: "http://127.0.0.1:8443", ChannelState: &noticeStateStore{}, NotificationRecipients: map[int64]string{42: "user:alice"}, NotificationBotCheck: func(context.Context, string, string) error { return nil }}, &captureRunner{})
	if err != nil {
		t.Fatal(err)
	}
	state := &telegramNoticeState{Owner: "user:alice", Since: time.Now().Add(-time.Hour), Sent: map[string]time.Time{noticeHash("outstanding"): time.Now().Add(-2 * time.Minute)}, Replies: map[string]telegramNoticeRef{}}
	for i := 0; i < maxTelegramNoticeRecords+10; i++ {
		state.Sent[fmt.Sprintf("new-%d", i)] = time.Now().Add(-time.Minute)
	}
	since := state.Since
	if err := h.saveNoticeState(t.Context(), 42, state); err != nil {
		t.Fatal(err)
	}
	source := func(context.Context, string) ([]notify.Event, error) {
		return []notify.Event{{ID: "outstanding", BotID: "worker", URL: "/bots/worker", At: time.Now(), Attention: true}}, nil
	}
	if err := h.DispatchNotifications(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if sends != 0 {
		t.Fatal("receipt pruning resent an outstanding alert")
	}
	loaded, err := h.loadNoticeState(t.Context(), 42, "user:alice")
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Since.Equal(since) {
		t.Fatal("receipt cleanup moved the activation watermark")
	}
	if len(loaded.Sent) != maxTelegramNoticeRecords+1 {
		t.Fatalf("inactive receipts not bounded: %d", len(loaded.Sent))
	}
	partial := func(context.Context, string, string) (notify.EventPage, error) {
		return notify.EventPage{Incomplete: true}, nil
	}
	if err := h.DispatchNotificationPages(t.Context(), partial); err != nil {
		t.Fatal(err)
	}
	loaded, err = h.loadNoticeState(t.Context(), 42, "user:alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.Sent[noticeHash("outstanding")]; !ok {
		t.Fatal("incomplete evidence read retired an active receipt")
	}
}
