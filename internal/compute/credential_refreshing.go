package compute

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Short-lived tokens, for providers that reject a static key.
//
// The Credential interface anticipated this from the start — its own
// doc names Vertex AI, which refuses API keys and wants an OAuth token
// refreshed roughly hourly — but only static header and query
// credentials existed, so those providers were not configurable at
// all.

// TokenSource mints a token and says when it stops working.
//
// A function rather than an interface because every implementation is
// one call: exchange a service-account JWT, read an instance metadata
// endpoint, shell out to a cloud CLI. The complexity is in the caching
// around it, which is here and shared.
//
// A zero expiry means "unknown", which is treated as short rather than
// forever — see defaultTokenLifetime.
type TokenSource func(ctx context.Context) (token string, expiresAt time.Time, err error)

const (
	// defaultRefreshMargin is how long before expiry a token is
	// considered spent.
	//
	// A token that expires while a request is in flight fails a call
	// that was valid when it started, and the failure looks like an
	// auth problem rather than a timing one. A minute covers a slow
	// generation request without refreshing constantly.
	defaultRefreshMargin = time.Minute

	// defaultTokenLifetime is assumed when a source does not say when
	// its token expires.
	//
	// Short rather than forever. A credential that never re-checks
	// would keep presenting a dead token indefinitely, and every
	// request would fail with no indication that the cause was age.
	defaultTokenLifetime = 5 * time.Minute
)

// RefreshingCredential caches a short-lived token and renews it.
type RefreshingCredential struct {
	source TokenSource
	header string
	prefix string
	margin time.Duration

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewRefreshingCredential wraps a token source.
//
// Header defaults to Authorization and prefix to "Bearer ", which is
// what Vertex and most OAuth providers want; a provider wanting the
// bare token passes an empty prefix.
func NewRefreshingCredential(source TokenSource, header, prefix string) *RefreshingCredential {
	if header == "" {
		header = DefaultAuthHeader
	}
	return &RefreshingCredential{
		source: source,
		header: header,
		prefix: prefix,
		margin: defaultRefreshMargin,
	}
}

// NewBearerTokenCredential is the common case: an OAuth bearer token.
func NewBearerTokenCredential(source TokenSource) *RefreshingCredential {
	return NewRefreshingCredential(source, "Authorization", "Bearer ")
}

// Apply attaches a live token, minting one if the cached token is
// missing or nearly expired.
func (c *RefreshingCredential) Apply(ctx context.Context, req *http.Request) error {
	token, err := c.liveToken(ctx)
	if err != nil {
		return err
	}
	req.Header.Set(c.header, c.prefix+token)
	return nil
}

// liveToken returns a live token.
//
// THE LOCK IS HELD ACROSS THE REFRESH, deliberately. Concurrent turns
// would otherwise each see an expired token and each mint a new one —
// a stampede against a token endpoint that is usually rate-limited,
// and N-1 tokens thrown away. Serialising behind one refresh is what
// is wanted; the wait is bounded by the caller's context, and refresh
// happens once an hour rather than once a request.
func (c *RefreshingCredential) liveToken(ctx context.Context) (string, error) {
	if c == nil || c.source == nil {
		return "", errors.New("credential: no token source configured")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && time.Now().Add(c.margin).Before(c.expiresAt) {
		return c.token, nil
	}

	token, expiresAt, err := c.source(ctx)
	if err != nil {
		// The stale token is NOT reused. A refresh that failed says
		// nothing about whether the old token still works, and
		// presenting an expired one produces a 401 that reads as a
		// misconfigured key rather than a token-endpoint outage.
		//
		// The error is wrapped without the token, obviously — but also
		// without the source's own message being trusted to be
		// secret-free, which is why it is the caller's job to keep
		// credentials out of the errors they return.
		return "", fmt.Errorf("credential: refresh failed: %w", err)
	}

	if token == "" {
		return "", errors.New("credential: token source returned an empty token")
	}

	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(defaultTokenLifetime)
	}

	c.token, c.expiresAt = token, expiresAt
	return token, nil
}

// Expired reports whether the cached token is missing or spent. For
// tests and for `lobslaw doctor`; the Apply path does not need it.
func (c *RefreshingCredential) Expired() bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token == "" || !time.Now().Add(c.margin).Before(c.expiresAt)
}
