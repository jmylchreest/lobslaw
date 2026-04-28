package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchSubjectReadsClaimFromJSONResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer testtok" {
			t.Errorf("Authorization header = %q, want Bearer testtok", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"email":"alice@example.com","sub":"123"}`))
	}))
	t.Cleanup(srv.Close)

	p := ProviderConfig{
		Name:             "test",
		UserInfoEndpoint: srv.URL,
		SubjectClaim:     "email",
	}
	got, err := FetchSubject(context.Background(), p, &TokenResponse{AccessToken: "testtok"})
	if err != nil {
		t.Fatalf("FetchSubject: %v", err)
	}
	if got != "alice@example.com" {
		t.Errorf("subject = %q, want alice@example.com", got)
	}
}

func TestFetchSubjectHonoursTokenType(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token testtok" {
			t.Errorf("Authorization header = %q, want token testtok", got)
		}
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	}))
	t.Cleanup(srv.Close)

	p := ProviderConfig{
		Name:             "test",
		UserInfoEndpoint: srv.URL,
		SubjectClaim:     "login",
	}
	tok := &TokenResponse{AccessToken: "testtok", TokenType: "token"}
	got, err := FetchSubject(context.Background(), p, tok)
	if err != nil {
		t.Fatalf("FetchSubject: %v", err)
	}
	if got != "octocat" {
		t.Errorf("subject = %q, want octocat", got)
	}
}

func TestFetchSubjectCoercesNumericClaim(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":42}`))
	}))
	t.Cleanup(srv.Close)

	p := ProviderConfig{Name: "test", UserInfoEndpoint: srv.URL, SubjectClaim: "id"}
	got, err := FetchSubject(context.Background(), p, &TokenResponse{AccessToken: "x"})
	if err != nil {
		t.Fatalf("FetchSubject: %v", err)
	}
	if got != "42" {
		t.Errorf("subject = %q, want 42", got)
	}
}

func TestFetchSubjectFailsOnMissingClaim(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"different_field":"x"}`))
	}))
	t.Cleanup(srv.Close)

	p := ProviderConfig{Name: "test", UserInfoEndpoint: srv.URL, SubjectClaim: "email"}
	_, err := FetchSubject(context.Background(), p, &TokenResponse{AccessToken: "x"})
	if err == nil {
		t.Fatal("expected error when SubjectClaim is missing from response")
	}
	if !strings.Contains(err.Error(), "email") {
		t.Errorf("error should mention missing field name: %v", err)
	}
}

func TestFetchSubjectFailsOnHTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
	}))
	t.Cleanup(srv.Close)

	p := ProviderConfig{Name: "test", UserInfoEndpoint: srv.URL, SubjectClaim: "email"}
	_, err := FetchSubject(context.Background(), p, &TokenResponse{AccessToken: "bad"})
	if err == nil {
		t.Fatal("expected error on HTTP 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should report HTTP status: %v", err)
	}
}

func TestFetchSubjectRequiresEndpoint(t *testing.T) {
	t.Parallel()
	p := ProviderConfig{Name: "test", SubjectClaim: "email"}
	_, err := FetchSubject(context.Background(), p, &TokenResponse{AccessToken: "x"})
	if err == nil {
		t.Error("expected error when UserInfoEndpoint is empty")
	}
}

func TestFetchSubjectRequiresAccessToken(t *testing.T) {
	t.Parallel()
	p := ProviderConfig{
		Name:             "test",
		UserInfoEndpoint: "https://example.invalid",
		SubjectClaim:     "email",
	}
	_, err := FetchSubject(context.Background(), p, &TokenResponse{})
	if err == nil {
		t.Error("expected error when access token is empty")
	}
}
