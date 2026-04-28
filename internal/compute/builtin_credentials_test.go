package compute

import (
	"context"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/oauth"
)

func TestRegisterCredentialsBuiltinsRequiresTrackerAndService(t *testing.T) {
	t.Parallel()
	b := NewBuiltins()
	if err := RegisterCredentialsBuiltins(b, CredentialsConfig{}); err == nil {
		t.Error("expected error when Tracker + Service are nil")
	}
}

func TestOAuthStartRejectsUnknownProvider(t *testing.T) {
	t.Parallel()
	cfg := CredentialsConfig{
		Tracker:   oauth.NewTracker(nil),
		Providers: map[string]oauth.ProviderConfig{"google": oauth.Google()},
	}
	h := newOAuthStartHandler(cfg)
	_, code, err := h(context.Background(), map[string]string{"provider": "doesnotexist"})
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if code != 2 {
		t.Errorf("expected exit 2 for caller error, got %d", code)
	}
	if !strings.Contains(err.Error(), "doesnotexist") {
		t.Errorf("error should mention the bad provider name: %v", err)
	}
}

func TestOAuthStartRejectsMissingProvider(t *testing.T) {
	t.Parallel()
	cfg := CredentialsConfig{Tracker: oauth.NewTracker(nil)}
	h := newOAuthStartHandler(cfg)
	if _, _, err := h(context.Background(), map[string]string{}); err == nil {
		t.Error("expected error when provider arg is empty")
	}
}

func TestCredentialsGrantRequiresAllFields(t *testing.T) {
	t.Parallel()
	h := newCredentialsGrantHandler(CredentialsConfig{})
	_, code, err := h(context.Background(), map[string]string{
		"provider": "google",
		"subject":  "u@e",
		// missing skill
		"scopes": `["gmail.readonly"]`,
	})
	if err == nil {
		t.Fatal("expected error for missing skill")
	}
	if code != 2 {
		t.Errorf("expected caller-error exit 2, got %d", code)
	}
}

func TestCredentialsGrantRejectsEmptyScopes(t *testing.T) {
	t.Parallel()
	h := newCredentialsGrantHandler(CredentialsConfig{})
	_, _, err := h(context.Background(), map[string]string{
		"provider": "google",
		"subject":  "u@e",
		"skill":    "gws",
		"scopes":   `[]`,
	})
	if err == nil {
		t.Error("expected error when scopes is an empty array")
	}
}

func TestSplitScopePrefersTokenScope(t *testing.T) {
	t.Parallel()
	got := splitScope("gmail.readonly calendar.readonly", []string{"fallback"})
	if len(got) != 2 || got[0] != "gmail.readonly" || got[1] != "calendar.readonly" {
		t.Errorf("expected provider-narrowed scopes, got %v", got)
	}
	got = splitScope("", []string{"a", "b"})
	if len(got) != 2 || got[0] != "a" {
		t.Errorf("expected fallback when token scope empty, got %v", got)
	}
}

func TestCredentialsToolDefsCoverAllFiveBuiltins(t *testing.T) {
	t.Parallel()
	defs := CredentialsToolDefs()
	want := map[string]bool{
		"oauth_start": true, "oauth_status": true, "oauth_revoke": true,
		"credentials_grant": true, "credentials_revoke": true,
	}
	for _, d := range defs {
		delete(want, d.Name)
	}
	if len(want) > 0 {
		t.Errorf("missing tool defs: %v", want)
	}
}
