package compute

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetchHappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello from server"))
	}))
	defer srv.Close()

	b := NewBuiltins()
	// Use a client without SSRF guard because httptest binds to
	// 127.0.0.1 which the guard would reject.
	_ = RegisterFetchBuiltin(b, FetchConfig{HTTPClient: http.DefaultClient})
	fn, _ := b.Get("fetch_url")
	out, exit, err := fn(context.Background(), map[string]string{"url": srv.URL})
	if err != nil || exit != 0 {
		t.Fatalf("err=%v exit=%d", err, exit)
	}
	var payload struct {
		Body      string `json:"body"`
		Truncated bool   `json:"truncated"`
		FromCache bool   `json:"from_cache"`
	}
	_ = json.Unmarshal(out, &payload)
	if payload.Body != "hello from server" {
		t.Errorf("body = %q", payload.Body)
	}
	if payload.FromCache {
		t.Error("first hit shouldn't be from_cache")
	}

	// Second call hits cache.
	out, _, _ = fn(context.Background(), map[string]string{"url": srv.URL})
	_ = json.Unmarshal(out, &payload)
	if !payload.FromCache {
		t.Error("second hit should be from_cache")
	}
}

func TestFetchHTMLToPlain(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>x</title><style>body{color:red}</style></head><body><h1>Hello</h1><p>World</p><script>alert('x')</script></body></html>`))
	}))
	defer srv.Close()

	b := NewBuiltins()
	_ = RegisterFetchBuiltin(b, FetchConfig{HTTPClient: http.DefaultClient})
	fn, _ := b.Get("fetch_url")
	out, _, _ := fn(context.Background(), map[string]string{"url": srv.URL})
	var payload struct {
		Body string `json:"body"`
	}
	_ = json.Unmarshal(out, &payload)
	if !strings.Contains(payload.Body, "Hello") || !strings.Contains(payload.Body, "World") {
		t.Errorf("extracted body missing content: %q", payload.Body)
	}
	if strings.Contains(payload.Body, "alert") || strings.Contains(payload.Body, "color:red") {
		t.Errorf("scripts/styles should be stripped: %q", payload.Body)
	}
}

func TestFetchRejectsBadScheme(t *testing.T) {
	t.Parallel()
	b := NewBuiltins()
	_ = RegisterFetchBuiltin(b, FetchConfig{HTTPClient: http.DefaultClient})
	fn, _ := b.Get("fetch_url")
	_, _, err := fn(context.Background(), map[string]string{"url": "file:///etc/passwd"})
	if err == nil {
		t.Error("file:// should be rejected")
	}
}

func TestFetchSSRFGuardBlocksPrivate(t *testing.T) {
	t.Parallel()
	cases := []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "169.254.169.254"}
	for _, ip := range cases {
		if !isBlockedIP(net.ParseIP(ip)) {
			t.Errorf("%s should be blocked", ip)
		}
	}
}

func TestFetchSSRFGuardAllowsPublic(t *testing.T) {
	t.Parallel()
	cases := []string{"1.1.1.1", "8.8.8.8", "93.184.216.34"}
	for _, ip := range cases {
		if isBlockedIP(net.ParseIP(ip)) {
			t.Errorf("%s should be allowed", ip)
		}
	}
}

func TestFetchCacheTTL(t *testing.T) {
	t.Parallel()
	c := &fetchCache{
		ttl:     10 * time.Millisecond,
		maxSize: 10,
		entries: map[string]*fetchCacheEntry{},
	}
	c.set("k", "v")
	if _, ok := c.get("k"); !ok {
		t.Error("set/get failed")
	}
	time.Sleep(20 * time.Millisecond)
	if _, ok := c.get("k"); ok {
		t.Error("expired entry should not hit")
	}
}

func TestFetchCacheEviction(t *testing.T) {
	t.Parallel()
	c := &fetchCache{
		ttl:     time.Hour,
		maxSize: 2,
		entries: map[string]*fetchCacheEntry{},
	}
	c.set("a", "1")
	time.Sleep(2 * time.Millisecond)
	c.set("b", "2")
	time.Sleep(2 * time.Millisecond)
	c.set("c", "3")
	if _, ok := c.get("a"); ok {
		t.Error("oldest entry should have been evicted")
	}
	if _, ok := c.get("c"); !ok {
		t.Error("newest entry should remain")
	}
}
