package memory

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func refreshFixture(t *testing.T, cs *CredentialService, subject string) {
	t.Helper()
	if err := cs.Put(context.Background(), &PlaintextCredential{Provider: "example", Subject: subject, AccessToken: "old", RefreshToken: "original", ExpiresAt: time.Now().Add(-time.Minute), Scopes: []string{"read", "write"}, AllowedSkills: []string{"skill"}, AllowedScopesPerSkill: map[string][]string{"skill": {"read"}}}); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshPreservesConcurrentAccountChanges(t *testing.T) {
	for _, change := range []string{"grant", "revoke", "delete", "relink"} {
		t.Run(change, func(t *testing.T) {
			cs := newTestCredentialService(t)
			refreshFixture(t, cs, "user")
			entered, release := make(chan struct{}), make(chan struct{})
			result := make(chan error, 1)
			go func() {
				_, err := cs.IssueForSkill(context.Background(), "example", "user", "skill", func(context.Context, string) (string, string, int, string, error) {
					close(entered)
					<-release
					return "fresh", "rotated", 3600, "read", nil
				})
				result <- err
			}()
			<-entered
			var err error
			switch change {
			case "grant":
				err = cs.Grant(context.Background(), "example", "user", "other", []string{"write"})
			case "revoke":
				err = cs.Revoke(context.Background(), "example", "user", "skill")
			case "delete":
				err = cs.Delete(context.Background(), "example", "user")
			case "relink":
				refreshFixture(t, cs, "user")
			}
			close(release)
			if err != nil {
				t.Fatal(err)
			}
			err = <-result
			if change == "grant" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("obsolete/unauthorized refresh returned a token")
			}
			if change == "delete" {
				if _, err := cs.Get(context.Background(), "example", "user"); err == nil {
					t.Fatal("deleted account resurrected")
				}
				return
			}
			rec, err := cs.Get(context.Background(), "example", "user")
			if err != nil {
				t.Fatal(err)
			}
			if change == "relink" {
				if rec.RefreshToken != "original" {
					t.Fatal("relinked account overwritten")
				}
				return
			}
			if rec.RefreshToken != "rotated" {
				t.Fatal("new token lost during ACL edit")
			}
			if change == "grant" && len(rec.AllowedScopesPerSkill["other"]) != 1 {
				t.Fatal("grant lost")
			}
			if change == "revoke" && len(cs.ScopesAllowedForSkill(rec, "skill")) != 0 {
				t.Fatal("revocation undone")
			}
		})
	}
}

func TestCallerCancellationDoesNotDiscardRotation(t *testing.T) {
	cs := newTestCredentialService(t)
	refreshFixture(t, cs, "user")
	entered, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := cs.IssueForSkill(ctx, "example", "user", "skill", func(ctx context.Context, _ string) (string, string, int, string, error) {
			close(entered)
			<-release
			if err := ctx.Err(); err != nil {
				return "", "", 0, "", err
			}
			return "fresh", "rotated", 3600, "", nil
		})
		result <- err
	}()
	<-entered
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller error=%v", err)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := cs.Get(context.Background(), "example", "user")
		if err == nil && p.RefreshToken == "rotated" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("rotated token was discarded when caller left")
}

func TestUncertainRefreshIsNotRetriedAfterRestart(t *testing.T) {
	cs := newTestCredentialService(t)
	refreshFixture(t, cs, "user")
	var calls atomic.Int32
	refresh := func(context.Context, string) (string, string, int, string, error) {
		calls.Add(1)
		return "", "", 0, "", errors.New("response lost")
	}
	if _, err := cs.IssueForSkill(context.Background(), "example", "user", "skill", refresh); !errors.Is(err, ErrCredentialRefreshUncertain) {
		t.Fatal(err)
	}
	restarted, err := NewCredentialService(cs.raft, cs.store, cs.key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.IssueForSkill(context.Background(), "example", "user", "skill", refresh); !errors.Is(err, ErrCredentialRefreshUncertain) {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("uncertain refresh retried")
	}
	// A new authorization replaces the generation and clears the stuck attempt.
	refreshFixture(t, restarted, "user")
	if _, err := restarted.IssueForSkill(context.Background(), "example", "user", "skill", func(context.Context, string) (string, string, int, string, error) {
		return "fresh", "rotated", 3600, "", nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredRefreshClaimCannotBeTakenOver(t *testing.T) {
	cs := newTestCredentialService(t)
	refreshFixture(t, cs, "user")
	before, err := cs.loadCredential("example", "user")
	if err != nil {
		t.Fatal(err)
	}
	after := proto.Clone(before).(*lobslawv1.CredentialRecord)
	after.ClaimedBy = "crashed-attempt"
	after.RefreshDeadline = timestamppb.New(time.Now().Add(-time.Hour))
	if err := cs.claimCredential(context.Background(), before, after); err != nil {
		t.Fatal(err)
	}
	_, err = cs.IssueForSkill(context.Background(), "example", "user", "skill", func(context.Context, string) (string, string, int, string, error) {
		t.Error("provider contacted after abandoned attempt")
		return "", "", 0, "", nil
	})
	if !errors.Is(err, ErrCredentialRefreshUncertain) {
		t.Fatal(err)
	}
}

func TestCredentialGenerationRejectsDeleteRecreateABA(t *testing.T) {
	cs := newTestCredentialService(t)
	refreshFixture(t, cs, "user")
	old, err := cs.loadCredential("example", "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.Delete(context.Background(), "example", "user"); err != nil {
		t.Fatal(err)
	}
	refreshFixture(t, cs, "user")
	after := proto.Clone(old).(*lobslawv1.CredentialRecord)
	after.ClaimedBy = "stale-attempt"
	if err := cs.claimCredential(context.Background(), old, after); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("stale claim accepted: %v", err)
	}
}

func TestIndependentCredentialsCanRefreshConcurrently(t *testing.T) {
	cs := newTestCredentialService(t)
	refreshFixture(t, cs, "one")
	refreshFixture(t, cs, "two")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	results := make(chan error, 2)
	for _, subject := range []string{"one", "two"} {
		go func() {
			_, err := cs.IssueForSkill(ctx, "example", subject, "skill", func(ctx context.Context, _ string) (string, string, int, string, error) {
				entered <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return "", "", 0, "", ctx.Err()
				}
				return "fresh", "rotated", 3600, "", nil
			})
			results <- err
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("independent refresh blocked")
		}
	}
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func TestRefreshOmittedFieldsAndReducedScopes(t *testing.T) {
	for _, scope := range []string{"", "write"} {
		t.Run(scope, func(t *testing.T) {
			cs := newTestCredentialService(t)
			refreshFixture(t, cs, "user")
			issued, err := cs.IssueForSkill(context.Background(), "example", "user", "skill", func(context.Context, string) (string, string, int, string, error) {
				return "fresh", "", 3600, scope, nil
			})
			if scope == "" {
				if err != nil || issued.AccessToken != "fresh" {
					t.Fatalf("issue=%v err=%v", issued, err)
				}
			} else if err == nil {
				t.Fatal("issued removed scope")
			}
			stored, err := cs.Get(context.Background(), "example", "user")
			if err != nil {
				t.Fatal(err)
			}
			if stored.RefreshToken != "original" {
				t.Fatal("omitted refresh token erased original")
			}
		})
	}
}

func TestRefreshLegacyCredentialWithoutRevision(t *testing.T) {
	cs := newTestCredentialService(t)
	legacy, err := cs.encrypt(&PlaintextCredential{Provider: "example", Subject: "legacy", AccessToken: "old", RefreshToken: "original", ExpiresAt: time.Now().Add(-time.Minute), Scopes: []string{"read"}, AllowedSkills: []string{"skill"}, AllowedScopesPerSkill: map[string][]string{"skill": {"read"}}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := proto.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.store.Put(BucketCredentials, "example:legacy", raw); err != nil {
		t.Fatal(err)
	}
	issued, err := cs.IssueForSkill(context.Background(), "example", "legacy", "skill", func(context.Context, string) (string, string, int, string, error) {
		return "fresh", "rotated", 3600, "", nil
	})
	if err != nil || issued.AccessToken != "fresh" {
		t.Fatalf("legacy issue=%v err=%v", issued, err)
	}
	rec, err := cs.loadCredential("example", "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Revision != 2 || rec.ClaimedBy != "" {
		t.Fatalf("legacy record did not enter CAS protocol: revision=%d claim=%q", rec.Revision, rec.ClaimedBy)
	}
}
