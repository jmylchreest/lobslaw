package notify

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type fakeSink struct {
	channelType string
	mu          sync.Mutex
	delivered   []deliveredCall
	err         error
}

type deliveredCall struct {
	address string
	body    string
}

func (f *fakeSink) ChannelType() string { return f.channelType }
func (f *fakeSink) Deliver(_ context.Context, address, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.delivered = append(f.delivered, deliveredCall{address: address, body: body})
	return nil
}

func (f *fakeSink) calls() []deliveredCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]deliveredCall, len(f.delivered))
	copy(out, f.delivered)
	return out
}

type stubPrefs struct {
	records map[string]*lobslawv1.UserPreferences
}

func (s *stubPrefs) Get(_ context.Context, userID string) (*lobslawv1.UserPreferences, error) {
	p, ok := s.records[userID]
	if !ok {
		return nil, errors.New("not found")
	}
	return p, nil
}

func TestSendBroadcastsToEveryBoundChannel(t *testing.T) {
	t.Parallel()
	prefs := &stubPrefs{records: map[string]*lobslawv1.UserPreferences{
		"alice": {
			UserId: "alice",
			Channels: []*lobslawv1.UserChannelAddress{
				{Type: "telegram", Address: "111"},
				{Type: "rest", Address: "alice@example.com"},
			},
		},
	}}
	tg := &fakeSink{channelType: "telegram"}
	rest := &fakeSink{channelType: "rest"}
	svc := NewService(prefs, nil)
	_ = svc.RegisterSink(tg)
	_ = svc.RegisterSink(rest)

	err := svc.Send(context.Background(), Notification{
		UserID: "alice",
		Body:   "hello",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(tg.calls()) != 1 || tg.calls()[0].address != "111" || tg.calls()[0].body != "hello" {
		t.Errorf("telegram delivery wrong: %+v", tg.calls())
	}
	if len(rest.calls()) != 1 || rest.calls()[0].address != "alice@example.com" {
		t.Errorf("rest delivery wrong: %+v", rest.calls())
	}
}

func TestSendOriginatorOnlyDeliversOnThatChannel(t *testing.T) {
	t.Parallel()
	prefs := &stubPrefs{records: map[string]*lobslawv1.UserPreferences{
		"alice": {
			UserId: "alice",
			Channels: []*lobslawv1.UserChannelAddress{
				{Type: "telegram", Address: "111"},
				{Type: "rest", Address: "alice@example.com"},
			},
		},
	}}
	tg := &fakeSink{channelType: "telegram"}
	rest := &fakeSink{channelType: "rest"}
	svc := NewService(prefs, nil)
	_ = svc.RegisterSink(tg)
	_ = svc.RegisterSink(rest)

	err := svc.Send(context.Background(), Notification{
		UserID:            "alice",
		Body:              "reply",
		OriginatorChannel: "rest",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tg.calls()) != 0 {
		t.Errorf("telegram should NOT receive reply (originator was rest); got %+v", tg.calls())
	}
	if len(rest.calls()) != 1 {
		t.Errorf("rest should receive exactly one delivery; got %d", len(rest.calls()))
	}
}

func TestSendOriginatorFallsBackToOriginatorID(t *testing.T) {
	t.Parallel()
	// User has no prefs binding for telegram, but the inbound
	// message's OriginatorID is the chat_id we should reply to.
	prefs := &stubPrefs{records: map[string]*lobslawv1.UserPreferences{
		"alice": {UserId: "alice", Channels: nil},
	}}
	tg := &fakeSink{channelType: "telegram"}
	svc := NewService(prefs, nil)
	_ = svc.RegisterSink(tg)
	err := svc.Send(context.Background(), Notification{
		UserID:            "alice",
		Body:              "reply",
		OriginatorChannel: "telegram",
		OriginatorID:      "697225",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tg.calls()) != 1 || tg.calls()[0].address != "697225" {
		t.Errorf("expected fallback delivery to OriginatorID 697225; got %+v", tg.calls())
	}
}

func TestSendDropsExpired(t *testing.T) {
	t.Parallel()
	svc := NewService(&stubPrefs{}, nil)
	_ = svc.RegisterSink(&fakeSink{channelType: "telegram"})
	err := svc.Send(context.Background(), Notification{
		UserID:    "alice",
		Body:      "stale",
		ExpiresAt: time.Now().Add(-time.Minute),
	})
	if !errors.Is(err, ErrExpired) {
		t.Errorf("expected ErrExpired; got %v", err)
	}
}

func TestSendBroadcastErrorsWhenNoChannelsBound(t *testing.T) {
	t.Parallel()
	prefs := &stubPrefs{records: map[string]*lobslawv1.UserPreferences{
		"alice": {UserId: "alice"},
	}}
	svc := NewService(prefs, nil)
	_ = svc.RegisterSink(&fakeSink{channelType: "telegram"})
	err := svc.Send(context.Background(), Notification{UserID: "alice", Body: "hi"})
	if !errors.Is(err, ErrUserUnbound) {
		t.Errorf("expected ErrUserUnbound; got %v", err)
	}
}

func TestSendBroadcastSkipsUnregisteredChannelTypes(t *testing.T) {
	t.Parallel()
	prefs := &stubPrefs{records: map[string]*lobslawv1.UserPreferences{
		"alice": {
			UserId: "alice",
			Channels: []*lobslawv1.UserChannelAddress{
				{Type: "telegram", Address: "111"},
				{Type: "slack", Address: "U-alice"}, // no slack sink registered
			},
		},
	}}
	tg := &fakeSink{channelType: "telegram"}
	svc := NewService(prefs, nil)
	_ = svc.RegisterSink(tg)
	err := svc.Send(context.Background(), Notification{UserID: "alice", Body: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tg.calls()) != 1 {
		t.Error("telegram should still receive delivery despite slack having no sink")
	}
}

func TestSendBroadcastSurvivesPartialFailure(t *testing.T) {
	t.Parallel()
	prefs := &stubPrefs{records: map[string]*lobslawv1.UserPreferences{
		"alice": {
			UserId: "alice",
			Channels: []*lobslawv1.UserChannelAddress{
				{Type: "telegram", Address: "111"},
				{Type: "rest", Address: "alice@example.com"},
			},
		},
	}}
	tg := &fakeSink{channelType: "telegram"}
	rest := &fakeSink{channelType: "rest", err: errors.New("REST is down")}
	svc := NewService(prefs, nil)
	_ = svc.RegisterSink(tg)
	_ = svc.RegisterSink(rest)
	err := svc.Send(context.Background(), Notification{UserID: "alice", Body: "hi"})
	if err != nil {
		t.Errorf("partial-success broadcast should not error: %v", err)
	}
	if len(tg.calls()) != 1 {
		t.Error("telegram should receive delivery even though rest failed")
	}
}

func TestRegisterSinkRejectsBadInput(t *testing.T) {
	t.Parallel()
	svc := NewService(&stubPrefs{}, nil)
	if err := svc.RegisterSink(nil); err == nil {
		t.Error("nil sink should error")
	}
	if err := svc.RegisterSink(&fakeSink{channelType: ""}); err == nil {
		t.Error("empty channel type should error")
	}
}

func TestSendRejectsEmptyUserOrBody(t *testing.T) {
	t.Parallel()
	svc := NewService(&stubPrefs{}, nil)
	if err := svc.Send(context.Background(), Notification{Body: "hi"}); err == nil {
		t.Error("empty user_id should error")
	}
	if err := svc.Send(context.Background(), Notification{UserID: "alice"}); err == nil {
		t.Error("empty body should error")
	}
}
