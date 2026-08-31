package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/notify"
)

type stubNotifier struct {
	sent []notify.Notification
	err  error
}

func (s *stubNotifier) Send(_ context.Context, n notify.Notification) error {
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, n)
	return nil
}

func notifyTool(t *testing.T, svc Notifier) compute.BuiltinFunc {
	t.Helper()
	b := NewBuiltins()
	if err := RegisterNotifyBuiltins(b, NotifyConfig{Service: svc}); err != nil {
		t.Fatalf("register: %v", err)
	}
	fn, ok := b.Get("notify")
	if !ok {
		t.Fatal("notify is not registered")
	}
	return fn
}

// The text and the recipient both have to survive to the notifier. A
// notification delivered to the right person with the wrong words, or
// the right words to the wrong person, is worse than one that failed.
func TestNotifyPassesRecipientAndTextThrough(t *testing.T) {
	t.Parallel()

	svc := &stubNotifier{}
	fn := notifyTool(t, svc)
	if _, code, err := fn(context.Background(), map[string]string{
		"user_id": "user:alice", "text": "the build finished",
	}); err != nil || code != 0 {
		t.Fatalf("notify: code=%d err=%v", code, err)
	}
	if len(svc.sent) != 1 {
		t.Fatalf("sent %d notifications, want 1", len(svc.sent))
	}
	if svc.sent[0].UserID != "user:alice" {
		t.Errorf("UserID = %q, want user:alice", svc.sent[0].UserID)
	}
	if svc.sent[0].Body != "the build finished" {
		t.Errorf("Body = %q, want the message as written", svc.sent[0].Body)
	}
}

// Both arguments are required, and refusing is exit 2 — the model can
// fix an argument error, where exit 1 invites a retry of something
// that will fail identically.
func TestNotifyRequiresRecipientAndText(t *testing.T) {
	t.Parallel()

	svc := &stubNotifier{}
	fn := notifyTool(t, svc)
	for _, args := range []map[string]string{
		{"text": "no recipient"},
		{"user_id": "user:alice"},
		{"user_id": "", "text": ""},
	} {
		_, code, err := fn(context.Background(), args)
		if err == nil || code != 2 {
			t.Errorf("args %v were accepted (code=%d err=%v)", args, code, err)
		}
	}
	if len(svc.sent) != 0 {
		t.Errorf("%d notifications were sent despite refusal", len(svc.sent))
	}
}

// A delivery failure reaches the model rather than being swallowed.
// The agent often tells the user it has notified someone; doing that
// after a silent failure is a lie it had the information to avoid.
func TestNotifySurfacesADeliveryFailure(t *testing.T) {
	t.Parallel()

	fn := notifyTool(t, &stubNotifier{err: errors.New("no channel bound")})
	out, code, err := fn(context.Background(), map[string]string{
		"user_id": "user:alice", "text": "hello",
	})
	if err == nil && code == 0 {
		t.Errorf("a failed delivery reported success: %s", out)
	}
}

// No notifier means no tool, rather than a tool that fails on use.
func TestNotifyWithoutAServiceRegistersNothing(t *testing.T) {
	t.Parallel()

	b := NewBuiltins()
	_ = RegisterNotifyBuiltins(b, NotifyConfig{})
	if _, ok := b.Get("notify"); ok {
		t.Error("notify registered with no service behind it")
	}
}
