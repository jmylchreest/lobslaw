package egress

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stripe/smokescreen/pkg/smokescreen"
	smokeacl "github.com/stripe/smokescreen/pkg/smokescreen/acl/v1"
	"github.com/stripe/smokescreen/pkg/smokescreen/conntrack"
)

// roleHeader is the request header smokescreen reads to identify the
// caller. Set by every Client this provider produces — both on
// regular HTTP requests and (via ProxyConnectHeader) on the CONNECT
// preamble for HTTPS.
const roleHeader = "X-Lobslaw-Role"

// SmokescreenConfig configures the embedded forward-proxy provider.
// BindAddr is "127.0.0.1:0" by default; tests override to a known
// port. The ACL is built at boot from declared lobslaw config (see
// builder.go) and may be replaced atomically via SetACL on hot-reload.
type SmokescreenConfig struct {
	// BindAddr defaults to "127.0.0.1:0" — kernel-picked ephemeral
	// port on loopback. Operators with stricter requirements can
	// pin a port; the proxy never binds anywhere except loopback.
	BindAddr string

	// ACL is the role → allowed-hostnames map. Roles correspond to
	// the strings passed to egress.For/ForSkill/ForMCP/ForOAuth.
	ACL Rules

	// UpstreamProxy is set when lobslaw runs behind a corporate
	// proxy. Empty string = direct egress. Non-empty must be the
	// full URL ("http://corp-proxy:8080" or "https://...").
	UpstreamProxy string

	// Logger handles smokescreen's diagnostic output. Falls back to
	// a logrus logger that proxies into slog.Default if unset.
	Logger *slog.Logger

	// AllowPrivateRanges disables smokescreen's default RFC1918
	// blocklist. NEVER set in production — it's the difference
	// between "compromised process can SSRF the local network" and
	// "no it can't." Set only by tests that use httptest.Server
	// (which binds loopback) or by the very-narrow "I host my LLM
	// upstream on a private RFC1918 box" deployment, where the
	// operator accepts the SSRF risk in exchange for talking to
	// their own infra.
	AllowPrivateRanges bool

	// AllowRanges adds CIDR blocks to smokescreen's allow-list. The
	// loopback range 127.0.0.0/8 is denied by default even with
	// AllowPrivateRanges (it's flagged as "not global unicast"); a
	// test that needs to talk to httptest.Server adds the loopback
	// CIDR here. Operators in unusual deployments (LLM provider on
	// the same host as lobslaw) might also use this knob.
	AllowRanges []string

	// UDSPath, when non-empty, also serves the proxy on a Unix-domain
	// socket at the given path. Used by subprocesses launched into
	// their own network namespace (manifest network_isolation: true) —
	// they can't reach the parent's TCP loopback but inherit the mount
	// namespace and can dial the UDS. Empty = TCP-only (the default;
	// fine for skills without netns isolation).
	//
	// Path is created with mode 0660. The subprocess sees it via the
	// inherited mount namespace; Landlock must include the parent
	// directory in AllowedPaths for the subprocess to dial it.
	UDSPath string
}

// Rules is the role-to-allowlist map the ACL builder produces.
// Roles use slash-separated identifiers ("skill/gws-workspace",
// "gateway/telegram", "llm", etc). Hostnames use smokescreen glob
// syntax — "*.googleapis.com" matches all subdomains; "api.example.com"
// matches exactly.
type Rules struct {
	// Roles maps role name to allowed hostname patterns. Anything
	// not matching falls through to DefaultAllowedHosts (typically
	// empty in production = deny anything not declared).
	Roles map[string][]string

	// DefaultAllowedHosts handles requests with an unknown role —
	// usually empty (deny). Set to non-empty for permissive dev
	// modes where every role gets the same allowlist.
	DefaultAllowedHosts []string

	// Permissive set means: allow anything that isn't a private IP.
	// Used for the bootstrap "fetch_url" role when operators haven't
	// declared an explicit allowlist.
	Permissive map[string]bool
}

// SmokescreenProvider runs an embedded smokescreen instance on a
// loopback listener and returns http.Clients pre-routed through it.
// Each Client carries the X-Lobslaw-Role header so smokescreen
// matches the right ACL rule.
type SmokescreenProvider struct {
	bindAddr           string
	listener           net.Listener
	udsListener        net.Listener // nil when UDSPath wasn't set
	udsPath            string
	server             *http.Server
	proxyURL           *url.URL
	upstreamProxy      *url.URL
	allowPrivateRanges bool
	allowRanges        []string

	// activeACL is replaced atomically on hot-reload. The proxy's
	// EgressACL points at this; reload swaps the underlying *ACL.
	activeACL atomic.Pointer[smokeacl.ACL]

	logger *slog.Logger

	clientsMu sync.RWMutex
	clients   map[string]*http.Client
}

// NewSmokescreenProvider constructs the provider, starts the proxy
// listener, and returns a ready-to-use Provider. The caller must
// invoke Stop during shutdown to drain the listener cleanly.
func NewSmokescreenProvider(cfg SmokescreenConfig) (*SmokescreenProvider, error) {
	if cfg.BindAddr == "" {
		cfg.BindAddr = "127.0.0.1:0"
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	listener, err := net.Listen("tcp", cfg.BindAddr)
	if err != nil {
		return nil, fmt.Errorf("egress: listen on %q: %w", cfg.BindAddr, err)
	}
	addr := listener.Addr().(*net.TCPAddr)
	if !addr.IP.IsLoopback() {
		_ = listener.Close()
		return nil, fmt.Errorf("egress: refusing to bind on non-loopback %s", addr.IP)
	}

	proxyURL, _ := url.Parse(fmt.Sprintf("http://%s", addr.String()))

	var upstream *url.URL
	if cfg.UpstreamProxy != "" {
		upstream, err = url.Parse(cfg.UpstreamProxy)
		if err != nil {
			_ = listener.Close()
			return nil, fmt.Errorf("egress: parse UpstreamProxy %q: %w", cfg.UpstreamProxy, err)
		}
	}

	p := &SmokescreenProvider{
		bindAddr:           addr.String(),
		listener:           listener,
		proxyURL:           proxyURL,
		upstreamProxy:      upstream,
		logger:             logger,
		allowPrivateRanges: cfg.AllowPrivateRanges,
		allowRanges:        cfg.AllowRanges,
		clients:            make(map[string]*http.Client),
	}

	acl := buildSmokescreenACL(cfg.ACL)
	p.activeACL.Store(acl)

	scfg, err := buildSmokescreenConfig(p)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	scfg.Listener = listener

	proxy := smokescreen.BuildProxy(scfg)
	p.server = &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if serveErr := p.server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			logger.Error("egress: proxy server exited", "err", serveErr)
		}
	}()

	// Optional UDS listener for netns-isolated subprocesses.
	if cfg.UDSPath != "" {
		// Remove any stale socket from a prior crash so Listen
		// doesn't fail with "address already in use." The socket
		// is process-private (we're the only user), so unlinking
		// is safe.
		_ = os.Remove(cfg.UDSPath)
		udsListener, udsErr := net.Listen("unix", cfg.UDSPath)
		if udsErr != nil {
			_ = listener.Close()
			return nil, fmt.Errorf("egress: listen on UDS %q: %w", cfg.UDSPath, udsErr)
		}
		if chmodErr := os.Chmod(cfg.UDSPath, 0o660); chmodErr != nil {
			_ = udsListener.Close()
			_ = listener.Close()
			return nil, fmt.Errorf("egress: chmod UDS %q: %w", cfg.UDSPath, chmodErr)
		}
		p.udsListener = udsListener
		p.udsPath = cfg.UDSPath
		go func() {
			if serveErr := p.server.Serve(udsListener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				logger.Error("egress: UDS proxy server exited", "err", serveErr)
			}
		}()
		logger.Info("egress: smokescreen UDS listener started", "path", cfg.UDSPath)
	}

	logger.Info("egress: smokescreen proxy started",
		"bind", addr.String(),
		"roles", len(cfg.ACL.Roles),
		"upstream_proxy", cfg.UpstreamProxy,
	)
	return p, nil
}

// For returns a Client for the given role. Clients are cached per
// role for the lifetime of the provider — the underlying transport
// is heavy and reusing it keeps connection pooling effective.
func (p *SmokescreenProvider) For(role string) Client {
	p.clientsMu.RLock()
	c, ok := p.clients[role]
	p.clientsMu.RUnlock()
	if ok {
		return &smokeClient{client: c, role: role}
	}

	p.clientsMu.Lock()
	defer p.clientsMu.Unlock()
	if c, ok := p.clients[role]; ok {
		return &smokeClient{client: c, role: role}
	}
	c = p.buildClient(role)
	p.clients[role] = c
	return &smokeClient{client: c, role: role}
}

// SetACL replaces the live ACL atomically. Used by Phase E.6 to
// regenerate rules on config hot-reload without bouncing the proxy.
// Existing in-flight requests keep their pre-swap decisions — only
// new requests see the new rules.
func (p *SmokescreenProvider) SetACL(rules Rules) {
	p.activeACL.Store(buildSmokescreenACL(rules))
	p.logger.Info("egress: ACL hot-reloaded", "roles", len(rules.Roles))
}

// Stop shuts down the proxy listener(s). Idempotent.
func (p *SmokescreenProvider) Stop(ctx context.Context) error {
	if p.server == nil {
		return nil
	}
	err := p.server.Shutdown(ctx)
	if p.udsPath != "" {
		_ = os.Remove(p.udsPath)
	}
	return err
}

// UDSPath returns the Unix-domain-socket path the proxy listens on,
// or "" when no UDS listener was configured. Used by callers that
// need to bind-mount or reference the socket from a child process.
func (p *SmokescreenProvider) UDSPath() string { return p.udsPath }

// ProxyURL returns the URL subprocesses should use as HTTPS_PROXY
// WITHOUT a role embedded — for callers that handle role identity
// some other way. Most spawned subprocesses should use
// SubprocessProxyURL(role) instead so smokescreen's ACL receives a
// role identifier from the Proxy-Authorization header.
func (p *SmokescreenProvider) ProxyURL() *url.URL { return p.proxyURL }

// SubprocessProxyURL returns the URL form a spawned subprocess
// should set as HTTPS_PROXY. Encodes the role into the user-info:
// "http://skill%2Fgws-workspace:_@127.0.0.1:<port>". The subprocess's
// HTTP library puts that into Proxy-Authorization Basic, smokescreen
// extracts the role, and the right per-role ACL applies.
//
// The password field is the literal "_" — smokescreen's role
// extraction ignores it. Subprocesses that examine the env var see
// a non-secret string (the role isn't a secret).
func (p *SmokescreenProvider) SubprocessProxyURL(role string) string {
	encoded := url.QueryEscape(role)
	return fmt.Sprintf("http://%s:_@%s", encoded, p.bindAddr)
}

// buildClient constructs the per-role http.Client. The Transport
// uses ProxyConnectHeader to inject the role header into the CONNECT
// preamble (HTTPS path) and a wrapping RoundTripper to inject it on
// regular HTTP requests too.
func (p *SmokescreenProvider) buildClient(role string) *http.Client {
	transport := &http.Transport{
		Proxy:              http.ProxyURL(p.proxyURL),
		ProxyConnectHeader: http.Header{roleHeader: []string{role}},
		MaxIdleConns:       100,
		IdleConnTimeout:    90 * time.Second,
	}
	return &http.Client{
		Transport: &roleInjector{base: transport, role: role},
		Timeout:   DefaultTimeout,
	}
}

// roleInjector wraps a Transport to also inject the role header on
// non-CONNECT (plain HTTP) requests. ProxyConnectHeader covers HTTPS;
// this covers HTTP. Both routes need the header so smokescreen's
// RoleFromRequest doesn't see an empty value.
type roleInjector struct {
	base http.RoundTripper
	role string
}

func (r *roleInjector) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	if cloned.Header == nil {
		cloned.Header = http.Header{}
	}
	cloned.Header.Set(roleHeader, r.role)
	return r.base.RoundTrip(cloned)
}

// smokeClient is the per-role Client returned to call sites.
type smokeClient struct {
	client *http.Client
	role   string
}

func (c *smokeClient) HTTPClient() *http.Client { return c.client }
func (c *smokeClient) Role() string             { return c.role }

// roleFromRequest extracts the lobslaw role from the request, using
// two paths:
//
//  1. X-Lobslaw-Role header — set by in-process Go clients via the
//     roleInjector RoundTripper + ProxyConnectHeader.
//
//  2. Proxy-Authorization Basic — username field. Subprocess HTTP
//     libraries (curl, python requests, gws-cli, etc.) all set this
//     automatically when HTTPS_PROXY is configured with embedded
//     user-info: HTTPS_PROXY=http://role@127.0.0.1:port. The role
//     appears as the basic-auth username; we ignore the password
//     field (subprocesses can put anything there). This is how the
//     egress proxy authenticates spawned skills + MCP servers + any
//     binary the operator wires through HTTPS_PROXY env.
//
// Returns smokescreen.MissingRoleError when neither path yields a
// role — smokescreen treats that as a hard deny.
func roleFromRequest(req *http.Request) (string, error) {
	if role := req.Header.Get(roleHeader); role != "" {
		return role, nil
	}
	if user, _, ok := proxyBasicAuth(req); ok && user != "" {
		// Subprocesses URL-escape slashes in the role so the proxy
		// URL is valid; reverse that here so smokescreen sees the
		// canonical "skill/gws-workspace" form against its ACL.
		if decoded, err := decodeRole(user); err == nil {
			return decoded, nil
		}
		return user, nil
	}
	return "", smokescreen.MissingRoleError("no " + roleHeader + " header or Proxy-Authorization role")
}

// proxyBasicAuth extracts the basic-auth user/pass from a CONNECT
// or HTTP-proxy request. Mirrors http.Request.BasicAuth except it
// reads Proxy-Authorization rather than Authorization (proxy auth
// uses a different header per RFC 7235).
func proxyBasicAuth(req *http.Request) (string, string, bool) {
	auth := req.Header.Get("Proxy-Authorization")
	if auth == "" {
		return "", "", false
	}
	const prefix = "Basic "
	if len(auth) < len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(auth[len(prefix):])
	if err != nil {
		return "", "", false
	}
	cs := string(decoded)
	idx := strings.IndexByte(cs, ':')
	if idx < 0 {
		return cs, "", true
	}
	return cs[:idx], cs[idx+1:], true
}

// decodeRole reverses the URL-escaping subprocesses apply to the
// role identifier so smokescreen sees the canonical slash-separated
// form. Round-trips skill/gws-workspace ↔ skill%2Fgws-workspace.
func decodeRole(s string) (string, error) {
	// We only need to decode %xx escapes; queryunescape is cheap and
	// matches what url.UserPassword writes on the egress side.
	return url.QueryUnescape(s)
}

// buildSmokescreenConfig assembles the smokescreen.Config from our
// provider state. Wires the role extractor + the atomic ACL +
// optional upstream proxy. Logger bridges to slog via a logrus
// hook so smokescreen's existing log calls land in our structured
// pipeline.
func buildSmokescreenConfig(p *SmokescreenProvider) (*smokescreen.Config, error) {
	scfg := smokescreen.NewConfig()
	scfg.RoleFromRequest = roleFromRequest
	scfg.AllowMissingRole = false

	// EgressACL is an interface; wrap our atomic.Pointer so swaps
	// at runtime are picked up by the next request.
	scfg.EgressACL = &aclRouter{ptr: &p.activeACL}

	scfg.Log = logrusFromSlog(p.logger)
	scfg.UnsafeAllowPrivateRanges = p.allowPrivateRanges
	if len(p.allowRanges) > 0 {
		if err := scfg.SetAllowRanges(p.allowRanges); err != nil {
			return nil, fmt.Errorf("egress: SetAllowRanges: %w", err)
		}
	}

	if p.upstreamProxy != nil {
		// goproxy's upstream-proxy hook works via SetUpstreamProxyAddr
		// in newer smokescreen; with v0.0.4 the wiring is via the
		// underlying http.Transport on the proxy. For now we keep
		// the upstream-proxy path as a TODO — direct egress works
		// for any deployment that doesn't sit behind a corporate
		// proxy. Wire-up lands when we add the corporate-proxy
		// integration test.
		p.logger.Warn("egress: UpstreamProxy is configured but not yet wired; direct egress will be used",
			"upstream", p.upstreamProxy.String())
	}

	// smokescreen's StartWithConfig (the public binary entrypoint)
	// initializes ConnTracker before the listener loop. We bypass
	// that path and call BuildProxy directly, so we have to install
	// the tracker ourselves — every CONNECT-mode dial dereferences
	// it via NewInstrumentedConn and panics on a nil receiver.
	scfg.ConnTracker = conntrack.NewTracker(
		scfg.IdleTimeout,
		scfg.MetricsClient.StatsdClient,
		scfg.Log,
		scfg.ShuttingDown,
	)

	return scfg, nil
}

// aclRouter satisfies smokeacl.Decider by delegating to the current
// atomic-pointer ACL. Lets us swap the live ACL without rebuilding
// the smokescreen.Config or bouncing the listener.
type aclRouter struct {
	ptr *atomic.Pointer[smokeacl.ACL]
}

func (r *aclRouter) Decide(service, host string) (smokeacl.Decision, error) {
	cur := r.ptr.Load()
	if cur == nil {
		return smokeacl.Decision{Result: smokeacl.Deny, Reason: "egress: no ACL configured"}, nil
	}
	return cur.Decide(service, host)
}

// buildSmokescreenACL turns our role rules into a smokescreen.ACL.
// Each role becomes one Rule with Policy=Enforce (deny by default,
// allow only declared globs). Permissive roles get their own Rule
// with an "*" glob — used for the fetch_url default-permissive case.
func buildSmokescreenACL(rules Rules) *smokeacl.ACL {
	acl := &smokeacl.ACL{
		Rules:  make(map[string]smokeacl.Rule, len(rules.Roles)),
		Logger: logrusBridge(),
	}
	for role, hosts := range rules.Roles {
		acl.Rules[role] = smokeacl.Rule{
			Project:     "lobslaw",
			Policy:      smokeacl.Enforce,
			DomainGlobs: hosts,
		}
	}
	for role := range rules.Permissive {
		acl.Rules[role] = smokeacl.Rule{
			Project:     "lobslaw",
			Policy:      smokeacl.Enforce,
			DomainGlobs: []string{"*"},
		}
	}
	if len(rules.DefaultAllowedHosts) > 0 {
		acl.DefaultRule = &smokeacl.Rule{
			Project:     "lobslaw",
			Policy:      smokeacl.Enforce,
			DomainGlobs: rules.DefaultAllowedHosts,
		}
	}
	return acl
}

// logrusFromSlog and logrusBridge plumb smokescreen's logrus
// expectations into our slog pipeline. Smokescreen logs at info/
// warn/error; the bridge keeps every line structured-log-compatible.
func logrusFromSlog(_ *slog.Logger) *logrus.Logger {
	// MVP: a logrus logger discarding everything but routing fatals
	// up. Stripping the verbose connection-tracking lines from the
	// main log keeps node logs readable; full smokescreen logs live
	// only when LOBSLAW_LOG_LEVEL=debug. Future work: a Hook that
	// forwards to slog with structured fields.
	l := logrus.New()
	l.SetLevel(logrus.WarnLevel)
	return l
}

func logrusBridge() *logrus.Logger {
	l := logrus.New()
	l.SetLevel(logrus.WarnLevel)
	return l
}
