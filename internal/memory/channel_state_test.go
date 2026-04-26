package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestChannelStateRoundTrip(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	cs := NewChannelStateService(svc.raft, svc.store)

	ctx := context.Background()
	if err := cs.Put(ctx, "telegram", "offset", []byte("42")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := cs.Get(ctx, "telegram", "offset")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "42" {
		t.Errorf("got %q, want 42", got)
	}
}

func TestChannelStateGetMissingReturnsNotFound(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	cs := NewChannelStateService(svc.raft, svc.store)
	_, err := cs.Get(context.Background(), "telegram", "missing")
	if !errors.Is(err, types.ErrNotFound) {
		t.Errorf("expected types.ErrNotFound, got %v", err)
	}
}

func TestChannelStateRefusesEmptyChannelOrKey(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	cs := NewChannelStateService(svc.raft, svc.store)
	if err := cs.Put(context.Background(), "", "offset", []byte("x")); err == nil {
		t.Error("expected error for empty channel")
	}
	if err := cs.Put(context.Background(), "telegram", "", []byte("x")); err == nil {
		t.Error("expected error for empty key")
	}
}

func TestChannelStateRefusesColonInKey(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	cs := NewChannelStateService(svc.raft, svc.store)
	if err := cs.Put(context.Background(), "tele:gram", "offset", []byte("x")); err == nil {
		t.Error("expected error for colon in channel name (would break key composition)")
	}
}

func TestChannelStateMultipleChannelsCoexist(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	cs := NewChannelStateService(svc.raft, svc.store)
	ctx := context.Background()

	if err := cs.Put(ctx, "telegram", "offset", []byte("100")); err != nil {
		t.Fatal(err)
	}
	if err := cs.Put(ctx, "rest", "cursor", []byte("abc")); err != nil {
		t.Fatal(err)
	}
	tg, _ := cs.Get(ctx, "telegram", "offset")
	if string(tg) != "100" {
		t.Errorf("telegram offset wrong: %q", tg)
	}
	rest, _ := cs.Get(ctx, "rest", "cursor")
	if string(rest) != "abc" {
		t.Errorf("rest cursor wrong: %q", rest)
	}
}

func TestChannelStatePutOverwrites(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	cs := NewChannelStateService(svc.raft, svc.store)
	ctx := context.Background()
	_ = cs.Put(ctx, "telegram", "offset", []byte("1"))
	_ = cs.Put(ctx, "telegram", "offset", []byte("2"))
	got, _ := cs.Get(ctx, "telegram", "offset")
	if string(got) != "2" {
		t.Errorf("expected overwrite to 2, got %q", got)
	}
}
