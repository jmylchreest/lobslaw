package compute

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Vertex AI rejects API keys outright and wants an OAuth token
// refreshed roughly hourly. The Credential interface named that case
// from the start; only static header and query credentials existed, so
// those providers were not configurable at all.

func req(t *testing.T) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, "https://example.invalid/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// countingSource hands out a new token each call.
type countingSource struct {
	mu     sync.Mutex
	calls  int
	expiry time.Duration
	err    error
	block  chan struct{}
}

func (s *countingSource) source(ctx context.Context) (string, time.Time, error) {
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return "", time.Time{}, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return "", time.Time{}, s.err
	}
	exp := s.expiry
	if exp == 0 {
		exp = time.Hour
	}
	return "token-" + itoa(s.calls), time.Now().Add(exp), nil
}

func (s *countingSource) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestATokenIsFetchedAndAttached(t *testing.T) {
	t.Parallel()
	s := &countingSource{}
	c := NewBearerTokenCredential(s.source)

	r := req(t)
	if err := c.Apply(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer token-1" {
		t.Errorf("Authorization = %q", got)
	}
}

// A token good for another hour must not be re-minted on every
// request: token endpoints are rate-limited, and a turn makes many
// calls.
func TestALiveTokenIsReused(t *testing.T) {
	t.Parallel()
	s := &countingSource{}
	c := NewBearerTokenCredential(s.source)

	for range 20 {
		if err := c.Apply(context.Background(), req(t)); err != nil {
			t.Fatal(err)
		}
	}
	if s.count() != 1 {
		t.Errorf("minted %d tokens for 20 requests", s.count())
	}
}

// A token that expires while a request is in flight fails a call that
// was valid when it started, and the failure reads as an auth problem
// rather than a timing one.
func TestATokenNearingExpiryIsRenewedEarly(t *testing.T) {
	t.Parallel()
	// Expires inside the refresh margin, so it is spent on arrival.
	s := &countingSource{expiry: defaultRefreshMargin / 2}
	c := NewBearerTokenCredential(s.source)

	r1 := req(t)
	if err := c.Apply(context.Background(), r1); err != nil {
		t.Fatal(err)
	}
	r2 := req(t)
	if err := c.Apply(context.Background(), r2); err != nil {
		t.Fatal(err)
	}
	if s.count() != 2 {
		t.Errorf("minted %d tokens; a token inside the margin was reused", s.count())
	}
	if r1.Header.Get("Authorization") == r2.Header.Get("Authorization") {
		t.Error("the same token was attached twice despite being nearly expired")
	}
}

// Concurrent turns must not each mint a token: that is a stampede
// against a rate-limited endpoint, and all but one token is discarded.
func TestConcurrentRequestsMintOneToken(t *testing.T) {
	t.Parallel()
	s := &countingSource{block: make(chan struct{})}
	c := NewBearerTokenCredential(s.source)

	const n = 25
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = c.Apply(context.Background(), req(t))
		}(i)
	}
	// Let them all pile up on the refresh before releasing it.
	time.Sleep(20 * time.Millisecond)
	close(s.block)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if s.count() != 1 {
		t.Errorf("%d concurrent requests minted %d tokens", n, s.count())
	}
}

// --- failure --------------------------------------------------------

// A refresh that failed says nothing about whether the old token still
// works, and presenting an expired one produces a 401 that reads as a
// misconfigured key rather than a token-endpoint outage.
func TestAFailedRefreshDoesNotReuseTheStaleToken(t *testing.T) {
	t.Parallel()
	s := &countingSource{expiry: defaultRefreshMargin / 2}
	c := NewBearerTokenCredential(s.source)

	r1 := req(t)
	if err := c.Apply(context.Background(), r1); err != nil {
		t.Fatal(err)
	}
	first := r1.Header.Get("Authorization")

	s.mu.Lock()
	s.err = errors.New("the token endpoint is down")
	s.mu.Unlock()

	r2 := req(t)
	err := c.Apply(context.Background(), r2)
	if err == nil {
		t.Fatal("a failed refresh was reported as success")
	}
	if got := r2.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q; a stale token was attached anyway", got)
	}
	if got := r2.Header.Get("Authorization"); got == first {
		t.Error("the expired token was reused after a failed refresh")
	}
}

// An empty token is a source that failed without saying so. Attaching
// "Bearer " would produce a 401 nobody can explain.
func TestAnEmptyTokenIsRefused(t *testing.T) {
	t.Parallel()
	c := NewBearerTokenCredential(func(context.Context) (string, time.Time, error) {
		return "", time.Now().Add(time.Hour), nil
	})
	if err := c.Apply(context.Background(), req(t)); err == nil {
		t.Error("an empty token was accepted")
	}
}

// The token must never reach an error message. A credential in a log
// is a credential.
func TestAFailureDoesNotCarryTheToken(t *testing.T) {
	t.Parallel()
	const secret = "ya29.super-secret-token"
	c := NewBearerTokenCredential(func(context.Context) (string, time.Time, error) {
		return secret, time.Now().Add(defaultRefreshMargin / 2), nil
	})
	// Prime the cache, then fail the next refresh.
	if err := c.Apply(context.Background(), req(t)); err != nil {
		t.Fatal(err)
	}
	c2 := NewBearerTokenCredential(func(context.Context) (string, time.Time, error) {
		return "", time.Time{}, errors.New("refused")
	})
	err := c2.Apply(context.Background(), req(t))
	if err == nil {
		t.Fatal("expected a failure")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error carries the token: %v", err)
	}
}

// A source that hangs must be cancellable, or one unreachable token
// endpoint holds every turn open.
func TestAHangingSourceRespectsTheContext(t *testing.T) {
	t.Parallel()
	s := &countingSource{block: make(chan struct{})} // never closed
	c := NewBearerTokenCredential(s.source)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- c.Apply(ctx, req(t)) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a cancelled refresh reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Apply ignored the context and hung")
	}
}

func TestNoSourceIsAnError(t *testing.T) {
	t.Parallel()
	var c *RefreshingCredential
	if err := c.Apply(context.Background(), req(t)); err == nil {
		t.Error("a nil credential was accepted")
	}
	if err := NewBearerTokenCredential(nil).Apply(context.Background(), req(t)); err == nil {
		t.Error("a credential with no source was accepted")
	}
}

// --- shape ----------------------------------------------------------

// A provider wanting the bare token, or its own header, must be
// expressible — the point of the seam is that a driver never asks
// which kind of credential it holds.
func TestTheHeaderAndPrefixAreConfigurable(t *testing.T) {
	t.Parallel()
	s := &countingSource{}
	c := NewRefreshingCredential(s.source, "X-Goog-Auth", "")

	r := req(t)
	if err := c.Apply(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if got := r.Header.Get("X-Goog-Auth"); got != "token-1" {
		t.Errorf("X-Goog-Auth = %q, want the bare token", got)
	}
	if r.Header.Get("Authorization") != "" {
		t.Error("it also set Authorization")
	}
}

// A source that does not say when its token expires must not be
// treated as never expiring: the credential would keep presenting a
// dead token indefinitely.
func TestAnUnknownExpiryIsTreatedAsShort(t *testing.T) {
	t.Parallel()
	c := NewBearerTokenCredential(func(context.Context) (string, time.Time, error) {
		return "t", time.Time{}, nil
	})
	if err := c.Apply(context.Background(), req(t)); err != nil {
		t.Fatal(err)
	}
	if c.Expired() {
		t.Error("a freshly minted token reported as expired")
	}
	// It must have a real expiry, not the zero time — which would be
	// permanently in the past and refresh on every request.
	c.mu.Lock()
	exp := c.expiresAt
	c.mu.Unlock()
	if exp.IsZero() {
		t.Fatal("the zero expiry was stored verbatim")
	}
	if time.Until(exp) > time.Hour {
		t.Errorf("an unknown expiry became %v away; it should be short", time.Until(exp))
	}
}

// It satisfies the interface every driver takes, so a driver never
// learns which kind it holds.
var _ Credential = (*RefreshingCredential)(nil)
