package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// jwks is the over-the-wire JWK Set shape (RFC 7517 §5).
type jwks struct {
	Keys []jwkEntry `json:"keys"`
}

// jwkEntry is one JWK. Only the fields we actually consume are
// captured; extras ("use", "key_ops", etc.) are ignored.
type jwkEntry struct {
	Kty string `json:"kty"`
	Kid string `json:"kid,omitempty"`
	Alg string `json:"alg,omitempty"`

	// RSA public key parameters.
	N string `json:"n,omitempty"`
	E string `json:"e,omitempty"`

	// EC / OKP parameters.
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
}

// parsedKey is a ready-to-verify public key with the algorithm the
// IdP advertised it for. Stored in the cache so we don't re-parse
// on every validation.
type parsedKey struct {
	pub any
	alg string
}

// Errors surfaced up the stack.
var (
	// errJWKSRefreshRateLimited fires when the caller asked for a
	// force-refresh but a recent attempt already ran. Prevents a
	// stream of malicious tokens with unknown kid values from
	// hammering the IdP.
	errJWKSRefreshRateLimited = errors.New("jwks: force refresh rate-limited")
	errJWKSUnknownKID         = errors.New("jwks: unknown kid")
)

// JWKSCache fetches a JWK set from a URL and serves parsed public
// keys by kid. Safe for concurrent use. Refreshes lazily on first
// access + every refreshInterval; force-refreshes once when a token
// references an unknown kid (with a rate limit so a bad actor can't
// spam the IdP).
type JWKSCache struct {
	url             string
	client          *http.Client
	refreshInterval time.Duration
	forceRefreshMin time.Duration // rate-limit window for unknown-kid refreshes
	logger          *slog.Logger

	mu           sync.RWMutex
	keys         map[string]parsedKey
	fetchedAt    time.Time
	lastForceAt  time.Time
}

// JWKSConfig tunes a JWKSCache. Zero values pick sensible defaults.
type JWKSConfig struct {
	URL             string
	Client          *http.Client
	RefreshInterval time.Duration
	ForceRefreshMin time.Duration
	Logger          *slog.Logger
}

// NewJWKSCache constructs a cache. A nil return with a non-nil
// error means "URL was set but misconfigured"; nil + nil means
// "no JWKS configured" so callers can hand this back to the
// Validator and let it fall through to HS256 or ErrNoValidator.
func NewJWKSCache(cfg JWKSConfig) (*JWKSCache, error) {
	if cfg.URL == "" {
		return nil, nil
	}
	c := &JWKSCache{
		url:             cfg.URL,
		client:          cfg.Client,
		refreshInterval: cfg.RefreshInterval,
		forceRefreshMin: cfg.ForceRefreshMin,
		logger:          cfg.Logger,
		keys:            make(map[string]parsedKey),
	}
	if c.client == nil {
		c.client = &http.Client{Timeout: 10 * time.Second}
	}
	if c.refreshInterval <= 0 {
		c.refreshInterval = 10 * time.Minute
	}
	if c.forceRefreshMin <= 0 {
		c.forceRefreshMin = 30 * time.Second
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	return c, nil
}

// Get returns the parsed public key + its advertised alg. Cache
// miss triggers one rate-limited force-refresh (handles IdP key
// rotation); a still-missing kid after refresh returns
// errJWKSUnknownKID. Empty kid is accepted iff the set has exactly
// one key.
func (c *JWKSCache) Get(ctx context.Context, kid string) (any, string, error) {
	if c == nil {
		return nil, "", errors.New("jwks: cache not configured")
	}
	// Lazy initial fetch + age-based refresh under a read-lock peek.
	c.maybeRefresh(ctx, false)

	if key, ok := c.lookup(kid); ok {
		return key.pub, key.alg, nil
	}

	// Miss. Force one refresh (rate-limited) then look again.
	if err := c.maybeRefresh(ctx, true); err != nil && !errors.Is(err, errJWKSRefreshRateLimited) {
		return nil, "", err
	}
	if key, ok := c.lookup(kid); ok {
		return key.pub, key.alg, nil
	}
	return nil, "", errJWKSUnknownKID
}

// lookup is the read path — held under RLock. Empty kid returns the
// sole key if exactly one is cached (common for small IdPs that
// don't set kids); returns (_, false) when the cache holds multiple
// keys and no kid was supplied (ambiguous).
func (c *JWKSCache) lookup(kid string) (parsedKey, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if kid == "" {
		if len(c.keys) == 1 {
			for _, k := range c.keys {
				return k, true
			}
		}
		return parsedKey{}, false
	}
	k, ok := c.keys[kid]
	return k, ok
}

// maybeRefresh performs a refresh if the cache is stale (or when
// force is true, subject to forceRefreshMin). Stale-cache fallback
// on fetch failure: we log + keep serving the old keys rather than
// bringing down auth on a transient IdP hiccup.
func (c *JWKSCache) maybeRefresh(ctx context.Context, force bool) error {
	c.mu.RLock()
	age := time.Since(c.fetchedAt)
	sinceLastForce := time.Since(c.lastForceAt)
	empty := c.fetchedAt.IsZero()
	c.mu.RUnlock()

	switch {
	case empty:
		// no keys yet — must fetch
	case force:
		if sinceLastForce < c.forceRefreshMin {
			return errJWKSRefreshRateLimited
		}
	case age < c.refreshInterval:
		return nil
	}

	return c.fetchAndStore(ctx, force)
}

// fetchAndStore downloads + parses the JWKS and swaps the cache.
// On parse / HTTP error, the existing cache is left untouched and
// the error is logged; this preserves the "stale beats dead" policy
// for transient upstream issues.
func (c *JWKSCache) fetchAndStore(ctx context.Context, forceRefreshAttempt bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return fmt.Errorf("jwks: build request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Warn("jwks: fetch failed — keeping stale cache", "url", c.url, "err", err)
		return c.markRefreshAttempt(forceRefreshAttempt, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		c.logger.Warn("jwks: non-2xx — keeping stale cache",
			"url", c.url, "status", resp.StatusCode, "body", string(body))
		return c.markRefreshAttempt(forceRefreshAttempt, fmt.Errorf("jwks: HTTP %d", resp.StatusCode))
	}

	var js jwks
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&js); err != nil {
		c.logger.Warn("jwks: malformed response — keeping stale cache", "url", c.url, "err", err)
		return c.markRefreshAttempt(forceRefreshAttempt, fmt.Errorf("jwks: decode: %w", err))
	}

	parsed := make(map[string]parsedKey, len(js.Keys))
	for i, e := range js.Keys {
		pub, alg, err := parseJWK(e)
		if err != nil {
			// One bad key shouldn't poison the whole set — skip it.
			c.logger.Warn("jwks: skipping unparseable key",
				"url", c.url, "index", i, "kid", e.Kid, "err", err)
			continue
		}
		// Absent kid is unusual but legal. Key it by "" so lookup()'s
		// single-key fallback path can still find it.
		parsed[e.Kid] = parsedKey{pub: pub, alg: alg}
	}
	if len(parsed) == 0 {
		c.logger.Warn("jwks: fetched set produced no usable keys — keeping stale cache", "url", c.url)
		return c.markRefreshAttempt(forceRefreshAttempt, errors.New("jwks: no usable keys"))
	}

	c.mu.Lock()
	c.keys = parsed
	c.fetchedAt = time.Now()
	if forceRefreshAttempt {
		c.lastForceAt = c.fetchedAt
	}
	c.mu.Unlock()
	return nil
}

// markRefreshAttempt records a failed force-refresh attempt so the
// rate limiter sees activity even on failure. Otherwise repeated
// misses-against-broken-upstream would each pay a full fetch.
func (c *JWKSCache) markRefreshAttempt(forceRefreshAttempt bool, origErr error) error {
	if forceRefreshAttempt {
		c.mu.Lock()
		c.lastForceAt = time.Now()
		c.mu.Unlock()
	}
	return origErr
}

// parseJWK dispatches on the JWK's kty, returning the appropriate
// crypto.PublicKey along with the alg the IdP advertised for it.
func parseJWK(e jwkEntry) (any, string, error) {
	switch e.Kty {
	case "RSA":
		pub, err := parseRSAKey(e)
		if err != nil {
			return nil, "", err
		}
		alg := e.Alg
		if alg == "" {
			alg = "RS256" // conventional default for RSA JWKs
		}
		return pub, alg, nil
	case "EC":
		pub, alg, err := parseECKey(e)
		if err != nil {
			return nil, "", err
		}
		return pub, alg, nil
	case "OKP":
		pub, err := parseEdDSAKey(e)
		if err != nil {
			return nil, "", err
		}
		return pub, "EdDSA", nil
	default:
		return nil, "", fmt.Errorf("unsupported kty %q", e.Kty)
	}
}

// parseRSAKey builds a *rsa.PublicKey from the base64url-encoded
// modulus (n) and exponent (e). Both fields MUST be present.
func parseRSAKey(e jwkEntry) (*rsa.PublicKey, error) {
	if e.N == "" || e.E == "" {
		return nil, errors.New("RSA JWK missing n or e")
	}
	n, err := decodeB64URLBigInt(e.N)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(e.E, "="))
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}
	// The exponent is usually 3 bytes (65537 = 0x010001). Pad to 8
	// bytes for binary.BigEndian.Uint64 parsing; left-pad with zeros.
	padded := make([]byte, 8)
	copy(padded[8-len(eBytes):], eBytes)
	exp := binary.BigEndian.Uint64(padded)
	if exp == 0 || exp > 1<<31 {
		return nil, fmt.Errorf("invalid RSA exponent %d", exp)
	}
	return &rsa.PublicKey{N: n, E: int(exp)}, nil
}

// parseECKey builds an *ecdsa.PublicKey. Curve is derived from the
// "crv" parameter (P-256 / P-384 / P-521), and the advertised alg
// is inferred from the curve if not supplied.
func parseECKey(e jwkEntry) (*ecdsa.PublicKey, string, error) {
	if e.X == "" || e.Y == "" || e.Crv == "" {
		return nil, "", errors.New("EC JWK missing crv/x/y")
	}
	var curve elliptic.Curve
	var alg string
	switch e.Crv {
	case "P-256":
		curve = elliptic.P256()
		alg = "ES256"
	case "P-384":
		curve = elliptic.P384()
		alg = "ES384"
	case "P-521":
		curve = elliptic.P521()
		alg = "ES512"
	default:
		return nil, "", fmt.Errorf("unsupported EC curve %q", e.Crv)
	}
	if e.Alg != "" {
		alg = e.Alg
	}
	x, err := decodeB64URLBigInt(e.X)
	if err != nil {
		return nil, "", fmt.Errorf("decode x: %w", err)
	}
	y, err := decodeB64URLBigInt(e.Y)
	if err != nil {
		return nil, "", fmt.Errorf("decode y: %w", err)
	}
	// Skip the explicit on-curve check (elliptic.IsOnCurve is deprecated
	// and the SEC1-encoded replacement in crypto/ecdh doesn't compose
	// cleanly with ecdsa.PublicKey{X,Y}). ecdsa.Verify rejects off-curve
	// points internally — every token signed by a malformed IdP key
	// just fails validation, which is the safe outcome.
	return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, alg, nil
}

// parseEdDSAKey extracts the 32-byte Ed25519 public key. Only the
// Ed25519 curve is supported — Ed448 (the other OKP curve) isn't
// implemented by the stdlib.
func parseEdDSAKey(e jwkEntry) (ed25519.PublicKey, error) {
	if e.Crv != "Ed25519" {
		return nil, fmt.Errorf("unsupported OKP curve %q", e.Crv)
	}
	if e.X == "" {
		return nil, errors.New("Ed25519 JWK missing x")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(e.X, "="))
	if err != nil {
		return nil, fmt.Errorf("decode x: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("Ed25519 key wrong size: %d", len(b))
	}
	return ed25519.PublicKey(b), nil
}

// decodeB64URLBigInt decodes a base64url-encoded big-endian integer.
// JWK fields use the "unpadded" variant; tolerate padding just in
// case an IdP sends it.
func decodeB64URLBigInt(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(b), nil
}
