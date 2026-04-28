package memory

import (
	"context"
	"errors"
	"testing"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func newTestUserPrefsService(t *testing.T) *UserPrefsService {
	t.Helper()
	svc := newTestServiceStack(t)
	return NewUserPrefsService(svc.raft, svc.store)
}

func TestUserPrefsPutGetRoundTrip(t *testing.T) {
	t.Parallel()
	svc := newTestUserPrefsService(t)
	ctx := context.Background()
	in := &lobslawv1.UserPreferences{
		UserId:      "owner",
		DisplayName: "Alice",
		Timezone:    "Europe/London",
		Language:    "en-GB",
		Channels: []*lobslawv1.UserChannelAddress{
			{Type: "telegram", Address: "697225"},
		},
	}
	if err := svc.Put(ctx, in); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := svc.Get(ctx, "owner")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DisplayName != "Alice" || got.Timezone != "Europe/London" {
		t.Errorf("round-trip: %+v", got)
	}
	if len(got.Channels) != 1 || got.Channels[0].Type != "telegram" {
		t.Errorf("channels round-trip: %+v", got.Channels)
	}
	if got.CreatedAt == nil || got.UpdatedAt == nil {
		t.Error("CreatedAt + UpdatedAt should be stamped on first write")
	}
}

func TestUserPrefsPreservesCreatedAtAcrossUpdates(t *testing.T) {
	t.Parallel()
	svc := newTestUserPrefsService(t)
	ctx := context.Background()
	first := &lobslawv1.UserPreferences{UserId: "owner", Timezone: "UTC"}
	if err := svc.Put(ctx, first); err != nil {
		t.Fatal(err)
	}
	stored, _ := svc.Get(ctx, "owner")
	originalCreated := stored.CreatedAt.AsTime()

	second := &lobslawv1.UserPreferences{UserId: "owner", Timezone: "Europe/London"}
	if err := svc.Put(ctx, second); err != nil {
		t.Fatal(err)
	}
	stored, _ = svc.Get(ctx, "owner")
	if !stored.CreatedAt.AsTime().Equal(originalCreated) {
		t.Errorf("CreatedAt changed across updates: %v → %v", originalCreated, stored.CreatedAt.AsTime())
	}
	if stored.Timezone != "Europe/London" {
		t.Errorf("Timezone update lost: %q", stored.Timezone)
	}
}

func TestUserPrefsRejectsInvalidTimezone(t *testing.T) {
	t.Parallel()
	svc := newTestUserPrefsService(t)
	err := svc.Put(context.Background(), &lobslawv1.UserPreferences{
		UserId:   "owner",
		Timezone: "Not/A/Real/Zone",
	})
	if err == nil {
		t.Error("invalid IANA zone should be rejected at write time")
	}
}

func TestUserPrefsRejectsBadUserID(t *testing.T) {
	t.Parallel()
	svc := newTestUserPrefsService(t)
	cases := []string{"", "has:colon", "has/slash"}
	for _, id := range cases {
		err := svc.Put(context.Background(), &lobslawv1.UserPreferences{UserId: id})
		if err == nil {
			t.Errorf("user_id %q should be rejected", id)
		}
	}
}

func TestUserPrefsFindByChannelAddress(t *testing.T) {
	t.Parallel()
	svc := newTestUserPrefsService(t)
	ctx := context.Background()
	_ = svc.Put(ctx, &lobslawv1.UserPreferences{
		UserId: "alice",
		Channels: []*lobslawv1.UserChannelAddress{
			{Type: "telegram", Address: "111"},
			{Type: "slack", Address: "U-alice"},
		},
	})
	_ = svc.Put(ctx, &lobslawv1.UserPreferences{
		UserId: "bob",
		Channels: []*lobslawv1.UserChannelAddress{
			{Type: "telegram", Address: "222"},
		},
	})
	got, err := svc.FindByChannelAddress(ctx, "telegram", "111")
	if err != nil {
		t.Fatal(err)
	}
	if got.UserId != "alice" {
		t.Errorf("expected alice; got %q", got.UserId)
	}
	if _, err := svc.FindByChannelAddress(ctx, "telegram", "999"); err == nil {
		t.Error("unbound channel address should error")
	}
}

func TestUserPrefsDeleteRemovesRecord(t *testing.T) {
	t.Parallel()
	svc := newTestUserPrefsService(t)
	ctx := context.Background()
	_ = svc.Put(ctx, &lobslawv1.UserPreferences{UserId: "owner"})
	if err := svc.Delete(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, "owner"); !errors.Is(err, types.ErrNotFound) {
		t.Errorf("expected NotFound after delete, got %v", err)
	}
}

func TestUserPrefsListReturnsAll(t *testing.T) {
	t.Parallel()
	svc := newTestUserPrefsService(t)
	ctx := context.Background()
	_ = svc.Put(ctx, &lobslawv1.UserPreferences{UserId: "alice"})
	_ = svc.Put(ctx, &lobslawv1.UserPreferences{UserId: "bob"})
	all, err := svc.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2 records, got %d", len(all))
	}
}
