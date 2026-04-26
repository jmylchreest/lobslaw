package modelsdev

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFetcherFetchesAndCaches(t *testing.T) {
	t.Parallel()

	cat := Catalog{
		"openrouter": Provider{
			ID:  "openrouter",
			API: "https://openrouter.ai/api/v1",
			Models: map[string]Model{
				"minimax/minimax-m2.7": {
					ID:         "minimax/minimax-m2.7",
					ToolCall:   true,
					Modalities: Modalities{Input: []string{"text"}},
				},
			},
		},
	}
	body, _ := json.Marshal(cat)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	tmp := t.TempDir()
	f := &Fetcher{URL: srv.URL, CacheDir: tmp, MaxAge: time.Hour, HTTP: srv.Client()}

	c1, err := f.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c1["openrouter"]; !ok {
		t.Errorf("first fetch missing openrouter: %v", c1)
	}

	c2, err := f.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c2["openrouter"]; !ok {
		t.Error("cache hit should still return data")
	}
	if hits != 1 {
		t.Errorf("expected single HTTP fetch with warm cache; got %d hits", hits)
	}

	if _, err := os.Stat(filepath.Join(tmp, "modelsdev.json")); err != nil {
		t.Errorf("cache file missing: %v", err)
	}
}

func TestFetcherFallsBackToStaleCache(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	stalePath := filepath.Join(tmp, "modelsdev.json")
	staleCat := Catalog{"x": Provider{ID: "x", Models: map[string]Model{"m": {ID: "m"}}}}
	body, _ := json.Marshal(staleCat)
	if err := os.WriteFile(stalePath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(stalePath, old, old)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)

	f := &Fetcher{URL: srv.URL, CacheDir: tmp, MaxAge: 24 * time.Hour, HTTP: srv.Client()}
	cat, err := f.Fetch(context.Background())
	if err == nil {
		t.Error("expected wrapped error indicating stale-cache fallback")
	}
	if cat == nil {
		t.Fatal("expected stale catalog returned despite HTTP failure")
	}
	if _, ok := cat["x"]; !ok {
		t.Errorf("stale catalog missing expected provider: %v", cat)
	}
}

func TestLookupExactMatch(t *testing.T) {
	t.Parallel()
	cat := Catalog{
		"openrouter": Provider{ID: "openrouter", Models: map[string]Model{
			"minimax/minimax-m2.7": {ID: "minimax/minimax-m2.7", ToolCall: true},
		}},
	}
	m, ok := cat.Lookup("", "minimax/minimax-m2.7")
	if !ok || !m.ToolCall {
		t.Errorf("exact lookup failed: %v ok=%v", m, ok)
	}
}

func TestLookupSuffixFallback(t *testing.T) {
	t.Parallel()
	cat := Catalog{
		"opencode": Provider{ID: "opencode", Models: map[string]Model{
			"minimax-m2.7": {ID: "minimax-m2.7", ToolCall: true},
		}},
	}
	m, ok := cat.Lookup("", "minimax/minimax-m2.7")
	if !ok || m.ID != "minimax-m2.7" {
		t.Errorf("suffix-fallback lookup failed: %v ok=%v", m, ok)
	}
}

func TestLookupHintMatchesByAPIHost(t *testing.T) {
	t.Parallel()
	cat := Catalog{
		"openrouter": Provider{
			ID:  "openrouter",
			API: "https://openrouter.ai/api/v1",
			Models: map[string]Model{
				"minimax/minimax-m2.7": {ID: "minimax/minimax-m2.7", ToolCall: true},
			},
		},
	}
	m, ok := cat.Lookup("openrouter.ai", "minimax/minimax-m2.7")
	if !ok || !m.ToolCall {
		t.Errorf("hint-by-API-host failed: %v ok=%v", m, ok)
	}
}

func TestLookupAllReturnsEveryListing(t *testing.T) {
	t.Parallel()
	cat := Catalog{
		"openrouter": Provider{Models: map[string]Model{"minimax/minimax-m2.7": {ID: "openrouter-m27"}}},
		"opencode":   Provider{Models: map[string]Model{"minimax-m2.7": {ID: "opencode-m27"}}},
	}
	got := cat.LookupAll("minimax/minimax-m2.7")
	if len(got) != 2 {
		t.Errorf("expected 2 matches (exact + suffix); got %d: %v", len(got), got)
	}
}

func TestLookupReturnsFalseForUnknown(t *testing.T) {
	t.Parallel()
	cat := Catalog{"x": Provider{Models: map[string]Model{"a": {ID: "a"}}}}
	if _, ok := cat.Lookup("", "missing"); ok {
		t.Error("expected miss for unknown model")
	}
}
