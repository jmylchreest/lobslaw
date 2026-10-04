package push

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func subscription(t *testing.T, endpoint string) webpush.Subscription {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return webpush.Subscription{Endpoint: endpoint, Keys: webpush.Keys{P256dh: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), Auth: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 16))}}
}

func TestEncryptedDeliveryDeduplicatesAcrossRestartAndRemovesExpiredEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "push.json")
	requests, status := 0, 201
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(body, []byte("private outcome")) || r.Header.Get("Content-Encoding") != "aes128gcm" || !strings.HasPrefix(r.Header.Get("Authorization"), "vapid ") {
			t.Fatal("push was not encrypted and VAPID-authenticated")
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	s, err := Open(path, client)
	if err != nil {
		t.Fatal(err)
	}
	sub := subscription(t, "https://fcm.googleapis.com/test-endpoint")
	if err := s.Subscribe("user:alice", "alice-login", time.Now().Add(time.Hour), sub); err != nil {
		t.Fatal(err)
	}
	if err := s.Subscribe("user:bob", "bob-login", time.Now().Add(time.Hour), sub); err == nil {
		t.Fatal("another account stole the subscription")
	}
	e := Event{ID: "event-1", Title: "Tester", Body: "private outcome", URL: "/bots/tester", At: time.Now()}
	source := func(_ context.Context, owner string) ([]Event, error) {
		if owner != "user:alice" {
			t.Fatal("wrong recipient")
		}
		return []Event{e}, nil
	}
	if err := s.Dispatch(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	key := s.PublicKey()
	s, err = Open(path, client)
	if err != nil {
		t.Fatal(err)
	}
	if s.PublicKey() != key {
		t.Fatal("VAPID keys rotated on restart")
	}
	if err := s.Dispatch(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatal("delivered the same event twice after restart")
	}
	status = 410
	e.ID = "event-2"
	if err := s.Dispatch(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if len(s.data.Devices) != 0 {
		t.Fatal("expired endpoint kept retrying")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("push credentials not private")
	}
}

func TestTransientFailureRetriesWithoutDuplicateSuccessOrCrossUserRevocation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "push.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	sub := subscription(t, "https://web.push.apple.com/test")
	if err := s.Subscribe("user:alice", "login", time.Now().Add(time.Hour), sub); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("user:bob", sub.Endpoint); err != nil {
		t.Fatal(err)
	}
	if len(s.data.Devices) != 1 {
		t.Fatal("cross-user unsubscribe")
	}
	calls := 0
	status := 503
	s.send = func(context.Context, []byte, *webpush.Subscription, *webpush.Options) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	source := func(context.Context, string) ([]Event, error) { return []Event{{ID: "event", At: time.Now()}}, nil }
	if s.Dispatch(t.Context(), source) == nil {
		t.Fatal("delivery failure reported success")
	}
	_ = s.Dispatch(t.Context(), source)
	if calls != 1 {
		t.Fatal("no retry backoff")
	}
	s.retry = map[string]time.Time{}
	status = 201
	if err := s.Dispatch(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("failed delivery not retried")
	}
	if err := s.RemoveLogin("login"); err != nil {
		t.Fatal(err)
	}
	if len(s.data.Devices) != 0 {
		t.Fatal("logout did not revoke push")
	}
}

func TestPushSubscriptionRejectsSSRFAndMalformedKeys(t *testing.T) {
	for _, endpoint := range []string{"http://fcm.googleapis.com/push", "https://127.0.0.1/push", "https://fcm.googleapis.com.evil.test/push", "https://user@fcm.googleapis.com/push", "https://web.push.apple.com:123/push", "https://example.com/push"} {
		if ValidateSubscription(subscription(t, endpoint)) == nil {
			t.Fatalf("accepted unsafe endpoint %s", endpoint)
		}
	}
	sub := subscription(t, "https://updates.push.services.mozilla.com/test")
	if err := ValidateSubscription(sub); err != nil {
		t.Fatal(err)
	}
	sub.Keys.Auth = "invalid"
	if ValidateSubscription(sub) == nil {
		t.Fatal("accepted invalid key")
	}
}
