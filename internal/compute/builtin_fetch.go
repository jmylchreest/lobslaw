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

	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// FetchConfig wires the fetch_url builtin. Zero HTTPClient → the
// production egress.For("fetch_url") client wrapped in an SSRF guard.
// Tests may inject a stubbed client; that client is used unchanged
// (no SSRF wrapping) so test harnesses can hit httptest endpoints.
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
		Description: "Fetch the contents of any HTTP(S) URL — web pages, GitHub pages/raw files, online docs, APIs, RSS feeds. Returns body as plaintext plus extracted links. This is the RIGHT TOOL for \"check the repo on GitHub\", \"look at this URL\", \"what does this website say\" — NOT grep/read_file (those are local-filesystem only). Private-network hosts are blocked (SSRF guard). HTML is extracted to plain text; max_chars caps the body (default 10000, max 50000). Response JSON: {url, body, truncated, from_cache, links: [{text, url}]}. When summarising fetched content for the user, CITE relevant source URLs using markdown link syntax like [headline](https://...) so the user can click through.",
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
	// Three layers of defence:
	//   1. URL-level SSRF guard (this RoundTripper wrapper) refuses
	//      private-IP-resolving hostnames before the request leaves
	//      the process.
	//   2. egress.For("fetch_url") routes through the in-process
	//      smokescreen proxy, which enforces hostname ACLs (operator
	//      can lock fetch_url down via [security] fetch_url_allow_hosts).
	//   3. smokescreen's own IP-level filter blocks private ranges
	//      regardless of what DNS returns.
	// Layers 1 and 3 overlap; 2 is the operator-facing knob. Belt
	// AND braces because fetch_url is the highest-blast-radius
	// builtin — the agent can be talked into reaching any URL.
	base := egress.For("fetch_url").HTTPClient()
	wrapped := *base
	wrapped.Transport = &ssrfGuardTransport{base: base.Transport}
	return &wrapped
}

// ssrfGuardTransport is the SSRF guard layer in front of the egress
// client. Resolves the request host, refuses if any resolved IP is
// in a private range. DNS-rebind-safe because the host is checked
// here AND at the smokescreen layer; rebind between checks would
// have to cross both barriers.
type ssrfGuardTransport struct {
	base http.RoundTripper
}

func (s *ssrfGuardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if host := req.URL.Hostname(); host != "" {
		if err := ssrfGuard(req.Context(), host); err != nil {
			return nil, err
		}
	}
	return s.base.RoundTrip(req)
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
			return packFetchResult(raw, cached.body, cached.links, maxChars, true)
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

		plain, links := htmlToPlainWithLinks(string(body), resp.Header.Get("Content-Type"), raw)
		cache.set(raw, plain, links)
		return packFetchResult(raw, plain, links, maxChars, false)
	}
}

// fetchLink is an extracted <a href> anchor from a fetched page.
type fetchLink struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

func packFetchResult(fullURL, body string, links []fetchLink, maxChars int, fromCache bool) ([]byte, int, error) {
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
		"links":      links,
	})
	if err != nil {
		return nil, 1, err
	}
	return out, 0, nil
}

// htmlToPlainWithLinks strips tags + extracts <a href> anchors.
// Returns (plain text, up-to-N deduplicated links). Relative URLs
// are resolved against pageURL. Non-HTML bodies return the input
// unchanged and no links. Not a full parser — regex-based, good
// enough for news/blog pages; pages with heavy JS-rendered
// content would need a headless browser.
func htmlToPlainWithLinks(body, contentType, pageURL string) (string, []fetchLink) {
	if !strings.Contains(strings.ToLower(contentType), "html") {
		return body, nil
	}
	links := extractAnchors(body, pageURL)
	body = stripScriptStyle.ReplaceAllString(body, " ")
	body = htmlTagRe.ReplaceAllString(body, " ")
	body = htmlWhitespaceRe.ReplaceAllString(body, " ")
	return strings.TrimSpace(body), links
}

// extractAnchors pulls (text, url) pairs from <a href="…">text</a>
// spans. Caps at 40 deduplicated entries — plenty for news
// homepages, not enough to flood the model's context. Relative
// URLs resolve against pageURL; fragment-only (#anchor) and
// javascript:/mailto: links are dropped.
const maxExtractedLinks = 40

func extractAnchors(html, pageURL string) []fetchLink {
	base, berr := url.Parse(pageURL)
	matches := anchorRe.FindAllStringSubmatch(html, -1)
	out := make([]fetchLink, 0, len(matches))
	seen := map[string]bool{}
	for _, m := range matches {
		href := strings.TrimSpace(m[1])
		text := strings.TrimSpace(htmlTagRe.ReplaceAllString(m[2], " "))
		text = htmlWhitespaceRe.ReplaceAllString(text, " ")
		if href == "" || strings.HasPrefix(href, "#") ||
			strings.HasPrefix(href, "javascript:") ||
			strings.HasPrefix(href, "mailto:") {
			continue
		}
		resolved := href
		if berr == nil {
			if parsed, perr := url.Parse(href); perr == nil {
				resolved = base.ResolveReference(parsed).String()
			}
		}
		if text == "" {
			text = resolved
		}
		if len(text) > 200 {
			text = text[:200] + "…"
		}
		key := resolved + "|" + text
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, fetchLink{Text: text, URL: resolved})
		if len(out) >= maxExtractedLinks {
			break
		}
	}
	return out
}

// anchorRe matches <a ... href="URL" ...>inner text</a>. Supports
// both single- and double-quoted href; not bulletproof (malformed
// HTML slips through) but covers the 90% case.
var anchorRe = regexp.MustCompile(`(?is)<a[^>]+href=["']([^"']+)["'][^>]*>(.*?)</a>`)

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
	body     string
	links    []fetchLink
	cachedAt time.Time
}

func (c *fetchCache) get(key string) (*fetchCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Since(e.cachedAt) > c.ttl {
		delete(c.entries, key)
		return nil, false
	}
	return e, true
}

func (c *fetchCache) set(key, body string, links []fetchLink) {
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
	c.entries[key] = &fetchCacheEntry{body: body, links: links, cachedAt: time.Now()}
}
