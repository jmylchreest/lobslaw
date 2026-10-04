package push

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

func TestPushRejectsPreviousOwnerAfterEndpointRebind(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "push.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	sub := subscription(t, "https://fcm.googleapis.com/account-switch")
	if err := s.Subscribe("user:alice", "alice-login", time.Now().Add(time.Hour), sub); err != nil {
		t.Fatal(err)
	}
	sent := 0
	s.send = func(context.Context, []byte, *webpush.Subscription, *webpush.Options) (*http.Response, error) {
		sent++
		return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	source := func(_ context.Context, owner string) ([]Event, error) {
		if owner != "user:alice" {
			t.Fatal("unexpected owner")
		}
		if err := s.Remove("user:alice", sub.Endpoint); err != nil {
			return nil, err
		}
		if err := s.Subscribe("user:bob", "bob-login", time.Now().Add(time.Hour), sub); err != nil {
			return nil, err
		}
		return []Event{{ID: "private", Body: "Alice's private result", At: time.Now()}}, nil
	}
	if err := s.Dispatch(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if sent != 0 {
		t.Fatal("sent an old owner's payload to the rebound endpoint")
	}
}

func TestExpiredBindingCanBeReplacedWithoutReusingAudience(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "push.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	sub := subscription(t, "https://web.push.apple.com/account-switch")
	if err := s.Subscribe("user:alice", "old", time.Now().Add(time.Hour), sub); err != nil {
		t.Fatal(err)
	}
	old := s.Binding("user:alice", "old", sub.Endpoint)
	d := s.data.Devices[hash(sub.Endpoint)]
	d.Expires = time.Now().Add(-time.Second)
	s.data.Devices[hash(sub.Endpoint)] = d
	if err := s.Subscribe("user:bob", "new", time.Now().Add(time.Hour), sub); err != nil {
		t.Fatal(err)
	}
	if current := s.Binding("user:bob", "new", sub.Endpoint); current == "" || current == old {
		t.Fatal("reused the previous audience")
	}
	if s.Binding("user:alice", "old", sub.Endpoint) != "" {
		t.Fatal("old login still has a binding")
	}
}

func TestLatePushFailureCannotDeleteReplacementBinding(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "push.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	sub := subscription(t, "https://fcm.googleapis.com/late-ack")
	if err := s.Subscribe("user:alice", "old", time.Now().Add(time.Hour), sub); err != nil {
		t.Fatal(err)
	}
	s.send = func(context.Context, []byte, *webpush.Subscription, *webpush.Options) (*http.Response, error) {
		if err := s.Remove("user:alice", sub.Endpoint); err != nil {
			t.Fatal(err)
		}
		if err := s.Subscribe("user:bob", "new", time.Now().Add(time.Hour), sub); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusGone, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	source := func(context.Context, string) ([]Event, error) { return []Event{{ID: "event", At: time.Now()}}, nil }
	if err := s.Dispatch(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if s.Binding("user:bob", "new", sub.Endpoint) == "" {
		t.Fatal("late response deleted the new account's subscription")
	}
}

func TestLegacyPushStateKeepsKeysButRequiresNewBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "push.json")
	s, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := s.PublicKey()
	s.data.Devices["old"] = Device{Owner: "user:alice", Expires: time.Now().Add(time.Hour)}
	raw, err := json.Marshal(s.data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.PublicKey() != key || len(reopened.data.Devices) != 0 {
		t.Fatal("legacy state retained an unbound device or rotated VAPID keys")
	}
}
