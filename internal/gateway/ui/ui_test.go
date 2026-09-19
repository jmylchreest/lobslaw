package ui

import (
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func handlerOrSkip(t *testing.T) http.Handler {
	t.Helper()
	h, err := Handler()
	if errors.Is(err, ErrNotBuilt) {
		t.Skip("web assets not built; run `make web`")
	}
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	return h
}

func TestServesTheAppShell(t *testing.T) {
	t.Parallel()
	h := handlerOrSkip(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `<div id="root">`) {
		t.Error("the response is not the app shell")
	}
	if got := rec.Header().Get("Cache-Control"); got != noCache {
		t.Errorf("Cache-Control = %q, want %s", got, noCache)
	}
}

func TestDeepLinkFallsThroughToTheShell(t *testing.T) {
	t.Parallel()
	h := handlerOrSkip(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/chat", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; a reload on a console page 404s", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `<div id="root">`) {
		t.Error("the deep link did not serve the shell")
	}
}

func TestUnknownAPIPathsAreNotSwallowed(t *testing.T) {
	t.Parallel()
	h := handlerOrSkip(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/nonexistent", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — an API typo must not return the app shell", rec.Code)
	}
}

func TestRealAssetsAreServed(t *testing.T) {
	t.Parallel()
	h := handlerOrSkip(t)

	sub, err := fs.Sub(dist, distDir)
	if err != nil {
		t.Fatalf("sub: %v", err)
	}
	entries, err := fs.ReadDir(sub, "assets")
	if err != nil {
		t.Skipf("no assets directory in this build: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the build produced no assets")
	}
	name := "/assets/" + entries[0].Name()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, name, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d serving %s", rec.Code, name)
	}
	if strings.Contains(rec.Body.String(), `<div id="root">`) {
		t.Errorf("%s fell through to the app shell; every script tag would return HTML", name)
	}
}

func TestBuiltMatchesHandler(t *testing.T) {
	t.Parallel()
	_, err := Handler()
	if Built() && errors.Is(err, ErrNotBuilt) {
		t.Fatal("Built is true but Handler returned ErrNotBuilt")
	}
	if !Built() && !errors.Is(err, ErrNotBuilt) {
		t.Fatalf("Built is false but Handler returned %v", err)
	}
}
