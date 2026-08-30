package secrets

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// BootstrapSchemes are the two references that resolve against this
// machine and cannot be replaced by a provider.
//
// They are the floor the rest stands on. A provider's own credential
// resolves through them, or the first fetch needs a resolver that needs
// a secret; and cmd/lobslaw resolves memory.encryption.key_ref before
// node.New runs, which is before any wiring stage — including the one
// that would build a provider — exists at all.
var BootstrapSchemes = []string{"env", "file"}

// IsBootstrapScheme reports whether a scheme is one of the two that
// always resolve locally. Config validation uses it to refuse a
// provider that would shadow one.
func IsBootstrapScheme(s string) bool {
	s = normalise(s)
	return slices.Contains(BootstrapSchemes, s)
}

// DefaultCacheTTL is how long a resolved value is reused.
//
// A single boot resolves the same reference several times — a provider
// key is read by the chat driver, the capability probe and doctor — and
// without a cache each one is a separate vault round trip, which on a
// CLI-backed provider is a separate process.
const DefaultCacheTTL = 5 * time.Minute

// Resolver turns a reference into a secret.
//
// It is deliberately shaped as func(string) (string, error) at the
// edge, because that is the signature every existing seam already takes
// — node.APIKeyResolver, node.ChannelSecretResolver, mcp.SecretResolver
// — so nothing above it has to change.
type Resolver struct {
	providers map[string]Provider
	ttl       time.Duration

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	value     string
	expiresAt time.Time
}

// NewResolver builds a resolver over already-constructed providers,
// keyed by the label that is also their reference scheme.
func NewResolver(providers map[string]Provider, ttl time.Duration) *Resolver {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &Resolver{
		providers: providers,
		ttl:       ttl,
		cache:     make(map[string]cacheEntry),
	}
}

// Resolve returns the secret a reference names.
//
// env: and file: are handled by pkg/config.ResolveSecret unchanged, so
// every existing config keeps working and the bootstrap path has no new
// code in it. Anything else is a provider label.
func (r *Resolver) Resolve(ref string) (string, error) {
	return r.ResolveContext(context.Background(), ref)
}

// ResolveContext is Resolve with a caller-supplied context, for the
// paths that have one.
func (r *Resolver) ResolveContext(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	scheme, path, ok := strings.Cut(ref, ":")
	if !ok {
		// Same refusal as pkg/config: a literal is never a reference,
		// so a plaintext secret cannot be committed by accident.
		return "", fmt.Errorf("%w: %q (expected scheme:value)", types.ErrUnknownSecretScheme, ref)
	}
	if IsBootstrapScheme(scheme) {
		return config.ResolveSecret(ref)
	}

	if strings.TrimSpace(path) == "" {
		// "bw:" is a typo, not a request. Left alone it reaches the
		// backend as an empty item name and comes back as whatever that
		// CLI says about nothing, which is a long way from the config
		// line that caused it.
		return "", fmt.Errorf("%w: %q names provider %q but no path", types.ErrMissingSecret, ref, scheme)
	}
	provider, ok := r.providers[normalise(scheme)]
	if !ok {
		return "", fmt.Errorf("%w: %q; configured providers: %s",
			types.ErrUnknownSecretScheme, scheme, r.labels())
	}
	if v, ok := r.cached(ref); ok {
		return v, nil
	}
	v, err := provider.Fetch(ctx, path)
	if err != nil {
		return "", err
	}
	r.store(ref, v)
	return v, nil
}

// labels lists configured provider schemes for an error message. Sorted
// so the message does not change between boots on identical config.
func (r *Resolver) labels() string {
	if len(r.providers) == 0 {
		return "none (only env: and file: are available)"
	}
	out := make([]string, 0, len(r.providers))
	for k := range r.providers {
		out = append(out, k)
	}
	sortStrings(out)
	return strings.Join(out, ", ")
}

func (r *Resolver) cached(ref string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.cache[ref]
	if !ok || time.Now().After(e.expiresAt) {
		return "", false
	}
	return e.value, true
}

func (r *Resolver) store(ref, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[ref] = cacheEntry{value: value, expiresAt: time.Now().Add(r.ttl)}
}

// Bootstrap is the resolver used before any provider exists, and for
// provider credentials themselves. It is pkg/config.ResolveSecret with
// an error that explains the constraint rather than reporting an
// unknown scheme, because "unknown scheme: bw" is a confusing thing to
// read when bw is configured and working three lines further down.
func Bootstrap(ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	scheme, _, ok := strings.Cut(ref, ":")
	if ok && !IsBootstrapScheme(scheme) {
		return "", fmt.Errorf(
			"%w: %q resolves before any secret provider exists, so it must use env: or file: "+
				"(this applies to memory.encryption.key_ref, [cluster.mtls] and any "+
				"[[secrets.providers]] credential)",
			types.ErrUnknownSecretScheme, ref)
	}
	return config.ResolveSecret(ref)
}
