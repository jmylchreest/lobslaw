package egress

import (
	"net/http"
	"sync"
	"time"
)

// Client is one call site's HTTP client + role identity. The role
// is set by the factory and injected into every outbound request as
// the X-Lobslaw-Role header so the proxy can apply the right ACL.
//
// Implementations must NOT expose the underlying http.Client in a way
// that lets callers swap the Transport — that would defeat the
// chokepoint. HTTPClient returns the same instance the factory built.
type Client interface {
	// HTTPClient returns the configured client. The same instance is
	// returned every call — callers may keep a reference for the
	// lifetime of their subsystem.
	HTTPClient() *http.Client

	// Role is the identifier the proxy uses for ACL lookup
	// (e.g. "llm", "skill/gws-workspace", "fetch_url"). Exposed for
	// callers that want to log or audit the outbound role.
	Role() string
}

// Provider builds Clients on demand. Implementations:
//
//   - noopProvider:  test/no-egress-filter mode. Returns a vanilla
//     http.Client with a sane default timeout and no proxy.
//   - smokescreenProvider: production. Routes through the embedded
//     in-process smokescreen on /run/lobslaw/egress.sock.
//
// The active provider is set once at node boot via SetActiveProvider.
// Concurrent Get/Set is not supported — callers must arrange for
// SetActiveProvider to fire before any For() call.
type Provider interface {
	// For returns a Client wired for the given role. The role string
	// MUST appear in the proxy's ACL (or "fetch_url"-style permissive
	// roles) — unknown roles fail closed at request time.
	For(role string) Client
}

var (
	activeMu sync.RWMutex
	active   Provider = newNoopProvider()
)

// SetActiveProvider replaces the active provider. Called once at
// node boot from internal/node/wire_*. Calling more than once
// (e.g. config hot-reload) is supported but tests SHOULD prefer
// constructing a fresh Provider over swapping the global.
func SetActiveProvider(p Provider) {
	if p == nil {
		p = newNoopProvider()
	}
	activeMu.Lock()
	active = p
	activeMu.Unlock()
}

// activeProvider returns the current provider under a read lock so
// callers see a consistent value even if SetActiveProvider runs
// concurrently.
func activeProvider() Provider {
	activeMu.RLock()
	defer activeMu.RUnlock()
	return active
}

// For returns an egress Client for a stable role identifier
// (e.g. "llm", "fetch_url"). Callers that need a per-skill role
// use ForSkill which prefixes "skill/" so the ACL builder doesn't
// have to know about every skill name up front.
func For(role string) Client {
	return activeProvider().For(role)
}

// ForSkill returns a Client wired with role "skill/<name>". Used by
// the skill invoker to identify per-skill subprocess HTTPS calls.
func ForSkill(skillName string) Client {
	return activeProvider().For("skill/" + skillName)
}

// ForMCP returns a Client wired with role "mcp/<name>". Used by the
// MCP loader for upstream calls to a specific server.
func ForMCP(serverName string) Client {
	return activeProvider().For("mcp/" + serverName)
}

// ForOAuth returns a Client for oauth flows targeting a specific
// provider's token endpoint. Used by the credential service when
// rotating refresh tokens.
func ForOAuth(provider string) Client {
	return activeProvider().For("oauth/" + provider)
}

// DefaultTimeout is the http.Client.Timeout the noop provider
// applies when no explicit timeout is requested. Production
// providers may impose stricter caps via the proxy itself.
const DefaultTimeout = 30 * time.Second
