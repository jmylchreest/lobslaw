package memory

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/turn"
)

func TestConnectorSettingsAreDurableOwnedAndConditional(t *testing.T) {
	stack := newTestServiceStack(t)
	s := NewConnectorSettingsStore(stack.raft, stack.store, stack.store.key, "google-calendar")
	alice := turn.WithIdentity(context.Background(), turn.Identity{Principal: "user:alice"})
	bob := turn.WithIdentity(context.Background(), turn.Identity{Principal: "user:bob"})
	r, err := s.Get(alice)
	if err != nil || r.Revision != 0 {
		t.Fatalf("initial %v %v", r, err)
	}
	r.Data = []byte(`{"nickname":"Private family"}`)
	if err = s.Save(alice, r); err != nil {
		t.Fatal(err)
	}
	raw, err := stack.store.Get(BucketIntegrationSettings, r.Id)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("Private family")) {
		t.Fatal("settings persisted as plaintext")
	}
	if err = s.Save(alice, r); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("stale write %v", err)
	}
	if err = s.Save(bob, r); err == nil {
		t.Fatal("cross-owner write")
	}
	other, err := s.Get(bob)
	if err != nil || len(other.Data) != 0 {
		t.Fatal("cross-owner read")
	}
	reopened := NewConnectorSettingsStore(stack.raft, stack.store, stack.store.key, "google-calendar")
	got, err := reopened.Get(alice)
	if err != nil || string(got.Data) != string(r.Data) || got.Revision != 1 {
		t.Fatalf("persisted %v %v", got, err)
	}
	if _, err = s.Get(context.Background()); err == nil {
		t.Fatal("anonymous read")
	}
}
