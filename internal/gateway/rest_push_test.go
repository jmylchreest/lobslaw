package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/jmylchreest/lobslaw/internal/push"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestPushSubscriptionRequiresEnrollmentAndCookieCSRF(t *testing.T) {
	s := NewServer(RESTConfig{RequireAuth: true, Users: enrolledAlice()}, nil)
	var err error
	s.push, err = push.Open(filepath.Join(t.TempDir(), "push.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.logins.put(&loginSession{ID: "login", UserID: "alice", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_, key, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"endpoint": "https://fcm.googleapis.com/test", "keys": webpush.Keys{P256dh: key, Auth: base64.RawURLEncoding.EncodeToString(make([]byte, 16))}, "protocol_version": 1})
	for _, tc := range []struct {
		cookie, origin bool
		status         int
	}{{false, false, 401}, {true, false, 403}, {true, true, 200}} {
		r := httptest.NewRequest(http.MethodPost, "http://console/v1/push", bytes.NewReader(body))
		if tc.cookie {
			r.AddCookie(&http.Cookie{Name: LoginCookieName, Value: "login"})
		}
		if tc.origin {
			r.Header.Set("Origin", "http://console")
		}
		w := httptest.NewRecorder()
		s.handlePush(w, r)
		if w.Code != tc.status {
			t.Fatalf("%+v: got %d %s", tc, w.Code, w.Body)
		}
		if w.Code == 200 {
			var binding struct {
				ID string `json:"binding_id"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &binding); err != nil || len(binding.ID) != 32 {
				t.Fatal("missing push audience binding")
			}
		}
	}
	legacy, _ := json.Marshal(webpush.Subscription{Endpoint: "https://fcm.googleapis.com/test", Keys: webpush.Keys{P256dh: key, Auth: base64.RawURLEncoding.EncodeToString(make([]byte, 16))}})
	r := httptest.NewRequest(http.MethodPost, "http://console/v1/push", bytes.NewReader(legacy))
	r.AddCookie(&http.Cookie{Name: LoginCookieName, Value: "login"})
	r.Header.Set("Origin", "http://console")
	w := httptest.NewRecorder()
	s.handlePush(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatal("legacy client can register without audience support")
	}
}

func TestPushOnlyForAttentionAndRequestedOutcomes(t *testing.T) {
	bot := &pb.ConsoleBot{Id: "tester", DisplayName: "Tester"}
	for _, tc := range []struct {
		status, sender, err string
		want                bool
	}{
		{"done", "bot:chief", "", false},
		{"done", "schedule:daily:match", "", false},
		{"done", "schedule:daily:never", "", false},
		{"done", "schedule:daily:always", "", true},
		{"done", "notify:tester", "", true},
		{"waiting", "schedule:daily:never", "", true},
		{"failed", "schedule:daily:match", "tool failed", true},
		{"failed", "schedule:daily:always", "task x: TASK_APPROVAL_STATE_CANCELLED", false},
		{"pending", "schedule:daily:always", "", false},
	} {
		item := &pb.ConsoleInboxItem{Id: "item", Recipient: "tester", Sender: tc.sender, Status: tc.status, Error: tc.err, Result: "outcome", TaskId: "task-1", CompletedAt: time.Now().Format(time.RFC3339Nano)}
		event, got := inboxPushEvent(bot, item, &pb.TaskApprovalRecord{State: pb.TaskApprovalState_TASK_APPROVAL_STATE_WAITING})
		if got != tc.want {
			t.Fatalf("%+v: push=%t", tc, got)
		}
		if got && event.Icon != "/__agent-icons/tester" {
			t.Fatal("lost agent identity")
		}
		if got && tc.status == "waiting" && event.URL != "/approvals/task-1" {
			t.Fatal("attention did not deep-link to task")
		}
	}
}

func TestRunningScheduledWorkDoesNotRequestAttention(t *testing.T) {
	bot := &pb.ConsoleBot{Id: "worker"}
	item := &pb.ConsoleInboxItem{Id: "item", Status: "waiting", TaskId: "task", Sender: "schedule:check:match"}
	for _, state := range []pb.TaskApprovalState{pb.TaskApprovalState_TASK_APPROVAL_STATE_RUNNING, pb.TaskApprovalState_TASK_APPROVAL_STATE_READY, pb.TaskApprovalState_TASK_APPROVAL_STATE_COMPLETED} {
		if _, ok := inboxPushEvent(bot, item, &pb.TaskApprovalRecord{State: state}); ok {
			t.Fatalf("state %s sent a false attention notification", state)
		}
	}
	if _, ok := inboxPushEvent(bot, item, nil); ok {
		t.Fatal("inbox status used as proof of approval")
	}
}
