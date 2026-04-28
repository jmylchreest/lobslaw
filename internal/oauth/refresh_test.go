package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRefreshTokenSwapsAccessToken(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != "refresh_token" {
			t.Errorf("grant_type = %q", r.PostForm.Get("grant_type"))
		}
		if r.PostForm.Get("refresh_token") != "rotate-me" {
			t.Errorf("refresh_token = %q", r.PostForm.Get("refresh_token"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh","expires_in":3600,"scope":"gmail.readonly"}`))
	}))
	t.Cleanup(srv.Close)

	p := ProviderConfig{
		Name:               "test",
		DeviceAuthEndpoint: srv.URL + "/device",
		TokenEndpoint:      srv.URL,
		ClientID:           "cid",
	}
	tok, err := RefreshToken(context.Background(), p, "rotate-me")
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if tok.AccessToken != "fresh" {
		t.Errorf("access = %q", tok.AccessToken)
	}
	if tok.ExpiresIn != 3600 {
		t.Errorf("expires_in = %d", tok.ExpiresIn)
	}
}

func TestRefreshTokenSurfacesInvalidGrant(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"revoked"}`))
	}))
	t.Cleanup(srv.Close)

	p := ProviderConfig{
		Name:               "test",
		DeviceAuthEndpoint: srv.URL + "/device",
		TokenEndpoint:      srv.URL,
		ClientID:           "cid",
	}
	_, err := RefreshToken(context.Background(), p, "stale")
	if !errors.Is(err, ErrInvalidGrant) {
		t.Errorf("expected ErrInvalidGrant, got %v", err)
	}
}

func TestRefreshTokenRejectsEmptyRefreshToken(t *testing.T) {
	t.Parallel()
	p := ProviderConfig{
		Name:               "test",
		DeviceAuthEndpoint: "https://x.invalid/device",
		TokenEndpoint:      "https://x.invalid/token",
		ClientID:           "cid",
	}
	_, err := RefreshToken(context.Background(), p, "")
	if err == nil || !strings.Contains(err.Error(), "refresh_token") {
		t.Errorf("expected refresh_token error; got %v", err)
	}
}
