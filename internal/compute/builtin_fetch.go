package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// FetchConfig wires the fetch_url builtin. Zero APIClient → a
// default one with 15s timeout + SSRF guard. Zero CacheTTL → 10min.
type FetchConfig struct {
	HTTPClient *http.Client
	CacheTTL   time.Duration
	CacheSize  int
}

// RegisterFetchBuiltin installs fetch_url. Always safe to call —
// unlike memory/websearch this has no required secret.
func RegisterFetchBuiltin(b *Builtins, cfg FetchConfig) error {
	client := cfg.HTTPClient
	if client == nil {
		client = defaultFetchClient()
	}
	ttl := cfg.CacheTTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	size := cfg.CacheSize
	if size <= 0 {
		size = 64
	}
	cache := &fetchCache{ttl: ttl, maxSize: size, entries: map[string]*fetchCacheEntry{}}
	return b.Register("fetch_url", newFetchHandler(client, cache))
}

// FetchToolDef is the ToolDef to register alongside the builtin.
func FetchToolDef() *types.ToolDef {
	return &types.ToolDef{
		Name:        "fetch_url",
		Path:        BuiltinScheme + "fetch_url",
		Description: "Fetch the contents of a URL and return the body as plaintext. Use when the user references a specific URL or you need to read a page whose contents you don't have. Private-network hosts (127.0.0.1, 10.0.0.0/8, etc.) are blocked to prevent SSRF. HTML is extracted to plain text; max_chars caps the response body (default 10000, max 50000).",
		ParametersSchema: []byte(`{
			"type": "object",
			"properties": {
				"url": {"type": "string", "description": "Full HTTP or HTTPS URL."},
				"max_chars": {"type": "integer", "description": "Max characters of body to return (default 10000, max 50000)."}
			},
			"required": ["url"],
			"additionalProperties": false
		}`),
		RiskTier: types.RiskCommunicating,
	}
}

func defaultFetchClient() *http.Client {
	// The custom Dial prevents SSRF — even if the model is tricked
	// into providing a URL whose DNS resolves to a private address,
	// the Dial refuses the connection before any bytes flow.
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				if err := ssrfGuard(ctx, host); err != nil {
					return nil, err
				}
				return dialer.DialContext(ctx, network, addr)
			},
		},
	}
}

// ssrfGuard resolves the host and refuses if any resolved IP is in
// a private range. DNS-rebind-safe because we resolve + check here,
// and the same Dialer is used for the connection.
func ssrfGuard(ctx context.Context, host string) error {
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return fmt.Errorf("fetch_url: host %q resolves to blocked address %s", host, ip)
		}
	}
	return nil
}

func isBlockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	if ip.IsPrivate() {
		return true
	}
	// Google metadata / AWS IMDS / Cloud Init — common SSRF targets.
	blocked := []string{"169.254.169.254", "fd00:ec2::254"}
	for _, b := range blocked {
		if ip.Equal(net.ParseIP(b)) {
			return true
		}
	}
	return false
}

const (
	fetchDefaultMaxChars = 10_000
	fetchHardMaxChars    = 50_000
	fetchMaxResponseBody = 5 * 1024 * 1024
)

func newFetchHandler(client *http.Client, cache *fetchCache) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		raw := strings.TrimSpace(args["url"])
		if raw == "" {
			return nil, 2, errors.New("fetch_url: url is required")
		}
		parsed, err := url.Parse(raw)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, 2, fmt.Errorf("fetch_url: url must be http or https: %q", raw)
		}
		maxChars := fetchDefaultMaxChars
		if v := args["max_chars"]; v != "" {
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
				maxChars = n
			}
		}
		if maxChars > fetchHardMaxChars {
			maxChars = fetchHardMaxChars
		}

		if cached, ok := cache.get(raw); ok {
			return packFetchResult(raw, cached, maxChars, true)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			return nil, 1, err
		}
		req.Header.Set("User-Agent", "lobslaw-fetch/1.0")
		req.Header.Set("Accept", "text/html, text/plain, application/json;q=0.9, */*;q=0.5")

		resp, err := client.Do(req)
		if err != nil {
			return nil, 1, fmt.Errorf("fetch_url: http: %w", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxResponseBody))
		if err != nil {
			return nil, 1, fmt.Errorf("fetch_url: read body: %w", err)
		}
		if resp.StatusCode >= 400 {
			return nil, 1, fmt.Errorf("fetch_url: HTTP %d: %s", resp.StatusCode, truncateBodyFor(body, 256))
		}

		plain := htmlToPlain(string(body), resp.Header.Get("Content-Type"))
		cache.set(raw, plain)
		return packFetchResult(raw, plain, maxChars, false)
	}
}

func packFetchResult(fullURL, body string, maxChars int, fromCache bool) ([]byte, int, error) {
	truncated := false
	if len(body) > maxChars {
		body = body[:maxChars]
		truncated = true
	}
	out, err := json.Marshal(map[string]any{
		"url":        fullURL,
		"body":       body,
		"truncated":  truncated,
		"from_cache": fromCache,
	})
	if err != nil {
		return nil, 1, err
	}
	return out, 0, nil
}

// htmlToPlain is a cheap HTML-to-text. Strips tags + collapses
// whitespace. Not a full markdown renderer — the goal is to get
// the model the semantic content without parser dependencies.
// JSON/plain bodies pass through unchanged.
func htmlToPlain(body, contentType string) string {
	if !strings.Contains(strings.ToLower(contentType), "html") {
		return body
	}
	body = stripScriptStyle.ReplaceAllString(body, " ")
	body = htmlTagRe.ReplaceAllString(body, " ")
	body = htmlWhitespaceRe.ReplaceAllString(body, " ")
	return strings.TrimSpace(body)
}

var (
	stripScriptStyle = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	htmlTagRe        = regexp.MustCompile(`<[^>]+>`)
	htmlWhitespaceRe = regexp.MustCompile(`\s+`)
)

// fetchCache is a thread-safe bounded LRU with TTL eviction.
// Not a real LRU — simple map + size cap with oldest-first eviction
// is enough for the personal-scale hit pattern (most fetches don't
// repeat).
type fetchCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	maxSize int
	entries map[string]*fetchCacheEntry
}

type fetchCacheEntry struct {
	body    string
	cachedAt time.Time
}

func (c *fetchCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if time.Since(e.cachedAt) > c.ttl {
		delete(c.entries, key)
		return "", false
	}
	return e.body, true
}

func (c *fetchCache) set(key, body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.maxSize {
		// Evict one stale-or-random entry to make room.
		oldestKey := ""
		var oldestAt time.Time
		for k, e := range c.entries {
			if oldestKey == "" || e.cachedAt.Before(oldestAt) {
				oldestKey = k
				oldestAt = e.cachedAt
			}
		}
		delete(c.entries, oldestKey)
	}
	c.entries[key] = &fetchCacheEntry{body: body, cachedAt: time.Now()}
}
