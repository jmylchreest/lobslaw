package memory

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentCredentialServicesRefreshOnce(t *testing.T) {
	first := newTestCredentialService(t)
	second, err := NewCredentialService(first.raft, first.store, first.key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := first.Put(ctx, &PlaintextCredential{
		Provider: "example", Subject: "user", AccessToken: "old-access", RefreshToken: "old-refresh",
		ExpiresAt: time.Now().Add(-time.Minute), Scopes: []string{"read"}, AllowedSkills: []string{"skill"},
		AllowedScopesPerSkill: map[string][]string{"skill": {"read"}},
	}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	refresher := func(ctx context.Context, token string) (string, string, int, string, error) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return "", "", 0, "", ctx.Err()
		}
		return "fresh-access", "fresh-refresh", 3600, "read", nil
	}
	results := make(chan error, 2)
	go func() { _, err := first.IssueForSkill(ctx, "example", "user", "skill", refresher); results <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() { _, err := second.IssueForSkill(ctx, "example", "user", "skill", refresher); results <- err }()
	// Hold the first exchange open: a second service must wait for its result,
	// rather than sending the same refresh token to the provider again.
	select {
	case <-entered:
	case <-time.After(250 * time.Millisecond):
	}
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("provider received %d refreshes for one expired credential, want 1", got)
	}
}
