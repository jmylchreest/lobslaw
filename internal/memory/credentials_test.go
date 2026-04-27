package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func newTestCredentialService(t *testing.T) *CredentialService {
	t.Helper()
	svc := newTestServiceStack(t)
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cs, err := NewCredentialService(svc.raft, svc.store, key)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func TestCredentialPutGetRoundTripDecrypts(t *testing.T) {
	t.Parallel()
	cs := newTestCredentialService(t)
	ctx := context.Background()
	cred := &PlaintextCredential{
		Provider:     "google",
		Subject:      "user@example.com",
		AccessToken:  "ya29.access-token-xyz",
		RefreshToken: "1//refresh-token-abc",
		Scopes:       []string{"gmail.readonly", "calendar.readonly"},
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	if err := cs.Put(ctx, cred); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := cs.Get(ctx, "google", "user@example.com")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AccessToken != cred.AccessToken {
		t.Errorf("access token round-trip: got %q, want %q", got.AccessToken, cred.AccessToken)
	}
	if got.RefreshToken != cred.RefreshToken {
		t.Errorf("refresh token round-trip: got %q, want %q", got.RefreshToken, cred.RefreshToken)
	}
	if len(got.Scopes) != 2 {
		t.Errorf("scopes: %v", got.Scopes)
	}
}

func TestCredentialBucketBytesAreCiphertext(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	key, _ := crypto.GenerateKey()
	cs, _ := NewCredentialService(svc.raft, svc.store, key)
	ctx := context.Background()
	plain := "ya29.SUPER-SECRET-TOKEN-zzz"
	_ = cs.Put(ctx, &PlaintextCredential{
		Provider: "google", Subject: "u@e", AccessToken: plain, RefreshToken: "r",
	})
	// Read raw bucket bytes — they must NOT contain the plaintext.
	raw, err := svc.store.Get(BucketCredentials, "google:u@e")
	if err != nil {
		t.Fatal(err)
	}
	if contains := containsBytes(raw, []byte(plain)); contains {
		t.Errorf("plaintext token leaked into bucket bytes — encryption broken")
	}
}

func TestCredentialGrantValidatesScopeSubset(t *testing.T) {
	t.Parallel()
	cs := newTestCredentialService(t)
	ctx := context.Background()
	_ = cs.Put(ctx, &PlaintextCredential{
		Provider:     "google",
		Subject:      "u@e",
		AccessToken:  "a",
		RefreshToken: "r",
		Scopes:       []string{"gmail.readonly"},
	})
	// Granting a scope the credential doesn't have must fail.
	err := cs.Grant(ctx, "google", "u@e", "gws-workspace", []string{"gmail.send"})
	if err == nil {
		t.Error("expected error granting scope not in credential's scopes")
	}
	// Granting a subset must succeed.
	if err := cs.Grant(ctx, "google", "u@e", "gws-workspace", []string{"gmail.readonly"}); err != nil {
		t.Fatalf("subset grant should succeed: %v", err)
	}
	got, _ := cs.Get(ctx, "google", "u@e")
	if !contains(got.AllowedSkills, "gws-workspace") {
		t.Error("skill should be in AllowedSkills after Grant")
	}
	if scopes := got.AllowedScopesPerSkill["gws-workspace"]; len(scopes) != 1 || scopes[0] != "gmail.readonly" {
		t.Errorf("scope subset wrong: %v", scopes)
	}
}

func TestCredentialRevokeClearsACL(t *testing.T) {
	t.Parallel()
	cs := newTestCredentialService(t)
	ctx := context.Background()
	_ = cs.Put(ctx, &PlaintextCredential{
		Provider: "google", Subject: "u@e", AccessToken: "a", RefreshToken: "r",
		Scopes: []string{"gmail.readonly"},
	})
	_ = cs.Grant(ctx, "google", "u@e", "skill-a", []string{"gmail.readonly"})
	_ = cs.Grant(ctx, "google", "u@e", "skill-b", []string{"gmail.readonly"})

	if err := cs.Revoke(ctx, "google", "u@e", "skill-a"); err != nil {
		t.Fatal(err)
	}
	got, _ := cs.Get(ctx, "google", "u@e")
	if contains(got.AllowedSkills, "skill-a") {
		t.Error("skill-a should be removed from AllowedSkills")
	}
	if !contains(got.AllowedSkills, "skill-b") {
		t.Error("skill-b should be untouched")
	}
	if _, present := got.AllowedScopesPerSkill["skill-a"]; present {
		t.Error("skill-a's scope entry should be removed")
	}
}

func TestCredentialDeleteRemovesRecord(t *testing.T) {
	t.Parallel()
	cs := newTestCredentialService(t)
	ctx := context.Background()
	_ = cs.Put(ctx, &PlaintextCredential{
		Provider: "google", Subject: "u@e", AccessToken: "a", RefreshToken: "r",
	})
	if err := cs.Delete(ctx, "google", "u@e"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Get(ctx, "google", "u@e"); !errors.Is(err, types.ErrNotFound) {
		t.Errorf("expected NotFound after delete, got %v", err)
	}
}

func TestCredentialKeyFormatRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct{ provider, subject string }{
		{"", "u@e"},
		{"google", ""},
		{"go:ogle", "u@e"},
		{"google", "u@e:x"},
	}
	for _, tc := range cases {
		if _, err := CredentialKey(tc.provider, tc.subject); err == nil {
			t.Errorf("CredentialKey(%q,%q) should error", tc.provider, tc.subject)
		}
	}
}

func TestCredentialNilKeyConstructionFails(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	if _, err := NewCredentialService(svc.raft, svc.store, crypto.Key{}); err == nil {
		t.Error("NewCredentialService should reject zero key (records would be unreadable on next boot)")
	}
}

func TestCredentialScopesAllowedForSkill(t *testing.T) {
	t.Parallel()
	cs := newTestCredentialService(t)
	ctx := context.Background()
	_ = cs.Put(ctx, &PlaintextCredential{
		Provider: "google", Subject: "u@e", AccessToken: "a", RefreshToken: "r",
		Scopes: []string{"gmail.readonly", "calendar.readonly"},
	})
	_ = cs.Grant(ctx, "google", "u@e", "gws", []string{"gmail.readonly"})
	got, _ := cs.Get(ctx, "google", "u@e")
	scopes := cs.ScopesAllowedForSkill(got, "gws")
	if len(scopes) != 1 || scopes[0] != "gmail.readonly" {
		t.Errorf("ScopesAllowedForSkill = %v, want [gmail.readonly]", scopes)
	}
	// Skill not in AllowedSkills returns empty.
	if scopes := cs.ScopesAllowedForSkill(got, "ghost"); len(scopes) != 0 {
		t.Errorf("unknown skill should return empty, got %v", scopes)
	}
}

func containsBytes(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
