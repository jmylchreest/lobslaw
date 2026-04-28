package egress

import (
	"net/url"
	"strings"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

// ACLInputs is the boot-time aggregation of every config block that
// declares an outbound destination. The egress.Builder turns this
// into a Rules map: one role per declared call site, one allowlist
// per role.
//
// Inputs are read-only — Build doesn't mutate. Hot-reload constructs
// a fresh ACLInputs from the current config and calls Build again,
// then swaps via SmokescreenProvider.SetACL.
type ACLInputs struct {
	// Providers is [[compute.providers]] from config.toml. Each
	// provider's endpoint host becomes an allowed host under role
	// "llm" (and per-role "llm/<label>" for callers that want
	// fine-grained restriction later).
	Providers []config.ProviderConfig

	// Channels is [[gateway.channels]]. Telegram pollers/senders
	// always need api.telegram.org; webhook channels declare
	// per-channel upstreams.
	Channels []config.GatewayChannelConfig

	// MCPServerNetworks maps MCP server name → upstream hosts the
	// server is expected to reach. MCP servers are subprocesses
	// (lobslaw talks to them via stdio, not HTTP); these rules
	// govern THEIR outbound traffic, applied via HTTPS_PROXY env
	// when the subprocess spawns.
	MCPServerNetworks map[string][]string

	// SkillNetworks maps skill manifest label → declared upstream
	// hosts. Skills with empty networks get role "skill/<name>"
	// with no allowed hosts (effective deny — matches the manifest
	// declaring no network access).
	SkillNetworks map[string][]string

	// ClawhubBaseURL is the API endpoint for clawhub.ai. Empty when
	// the operator hasn't enabled clawhub installation.
	ClawhubBaseURL string

	// ClawhubBinaryHosts is the operator-declared allowlist for
	// binary download URLs (Phase B). Default is github.com release
	// hosts; operators can ratchet up.
	ClawhubBinaryHosts []string

	// FetchURLAllowHosts is the optional [compute.fetch] allow_hosts
	// list. Empty = default-permissive (any public host, smokescreen
	// blocks private IPs anyway). Non-empty = explicit allowlist.
	FetchURLAllowHosts []string

	// BinariesInstallHosts is the union of upstream hostnames declared
	// by the operator's [[binary]] entries (apt repos, brew CDN, pypi,
	// etc.). Used to seed the "binaries-install" role. Empty when no
	// binaries are declared — role isn't registered.
	BinariesInstallHosts []string

	// OAuthProviders maps provider name → its DeviceAuth + Token
	// endpoints. Egress under "oauth/<name>" is restricted to those
	// two hosts so a misconfigured client can't reach an arbitrary
	// IdP. Empty = no oauth roles registered.
	OAuthProviders map[string]OAuthEndpoints
}

// OAuthEndpoints carries the two URLs an OAuth device flow needs to
// reach. Only the host portion is used; smokescreen does host-level
// matching, not path matching.
type OAuthEndpoints struct {
	DeviceAuthEndpoint string
	TokenEndpoint      string
}

// Build aggregates the inputs into a Rules struct ready to feed into
// SmokescreenProvider. Each input source contributes one or more
// roles. Hosts are extracted from URLs; opaque hostnames pass through
// unchanged.
func Build(in ACLInputs) Rules {
	rules := Rules{
		Roles:      make(map[string][]string),
		Permissive: make(map[string]bool),
	}

	// LLM provider endpoints — collected under "llm" (broad), plus
	// per-label "llm/<label>" for future fine-grained restriction.
	llmHosts := uniqueHosts{}
	for _, p := range in.Providers {
		host := hostOf(p.Endpoint)
		if host == "" {
			continue
		}
		llmHosts.add(host)
		if p.Label != "" {
			rules.Roles["llm/"+p.Label] = []string{host}
		}
	}
	if len(llmHosts.list) > 0 {
		rules.Roles["llm"] = llmHosts.list
		// embedding shares the same set today — operators can
		// ratchet down by declaring an explicit embedding endpoint
		// in compute.embedder configuration when that lands.
		rules.Roles["embedding"] = llmHosts.list
	}

	// Gateway channels — Telegram is the only outbound HTTP channel
	// today (api.telegram.org). Webhook channels are INBOUND; they
	// don't need an outbound rule. Future channels with their own
	// upstream (Slack, Discord) extend this switch.
	for _, ch := range in.Channels {
		if ch.Type == "telegram" {
			rules.Roles["gateway/telegram"] = []string{"api.telegram.org"}
		}
	}

	// MCP servers — per-subprocess role. The lobslaw process itself
	// never makes HTTP calls under these roles; the role is applied
	// to the spawned MCP subprocess via HTTPS_PROXY env.
	for name, hosts := range in.MCPServerNetworks {
		rules.Roles["mcp/"+name] = hosts
	}

	// Skills — per-skill role from manifest's network: array.
	for skill, hosts := range in.SkillNetworks {
		role := "skill/" + skill
		if len(hosts) == 0 {
			// Empty manifest network — explicitly no allowed hosts.
			// We still register the role so requests don't hit the
			// default-deny "missing role" path; smokescreen reports
			// the deny with a more useful message.
			rules.Roles[role] = nil
			continue
		}
		rules.Roles[role] = hosts
	}

	// Clawhub installer — hardcoded host set (clawhub API + the
	// operator-declared binary hosts).
	if in.ClawhubBaseURL != "" {
		hosts := []string{hostOfOrSelf(in.ClawhubBaseURL)}
		hosts = append(hosts, in.ClawhubBinaryHosts...)
		if len(in.ClawhubBinaryHosts) == 0 {
			// Sensible defaults for github-hosted binaries — the
			// most common skill-install path.
			hosts = append(hosts, "github.com", "objects.githubusercontent.com", "*.githubusercontent.com")
		}
		rules.Roles["clawhub"] = hosts
	}

	if len(in.BinariesInstallHosts) > 0 {
		seen := make(map[string]struct{}, len(in.BinariesInstallHosts))
		hosts := make([]string, 0, len(in.BinariesInstallHosts))
		for _, h := range in.BinariesInstallHosts {
			if h == "" {
				continue
			}
			if _, dup := seen[h]; dup {
				continue
			}
			seen[h] = struct{}{}
			hosts = append(hosts, h)
		}
		rules.Roles["binaries-install"] = hosts
	}

	// OAuth IdP endpoints — one role per declared provider, scoped
	// to that provider's two hosts. The credentials builtins call
	// egress.For("oauth/<name>") so a misconfigured client (or a
	// compromised one) can't reach a different IdP.
	for name, ep := range in.OAuthProviders {
		hosts := uniqueHosts{}
		if h := hostOf(ep.DeviceAuthEndpoint); h != "" {
			hosts.add(h)
		}
		if h := hostOf(ep.TokenEndpoint); h != "" {
			hosts.add(h)
		}
		rules.Roles["oauth/"+name] = hosts.list
	}

	// fetch_url is the deliberately permissive role. Operators who
	// want it locked down declare an explicit allowlist.
	if len(in.FetchURLAllowHosts) > 0 {
		rules.Roles["fetch_url"] = in.FetchURLAllowHosts
	} else {
		rules.Permissive["fetch_url"] = true
	}

	return rules
}

// hostOf extracts the host portion from a URL. Returns "" if the URL
// can't be parsed or has no host. Used to turn config endpoints
// ("https://api.minimax.io/v1") into smokescreen allowlist entries
// ("api.minimax.io").
func hostOf(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	if u.Host != "" {
		return u.Hostname()
	}
	// Operators sometimes write bare hostnames in config — accept
	// those as-is when they round-trip through url.Parse without a
	// scheme.
	return strings.TrimSpace(rawURL)
}

// hostOfOrSelf returns hostOf when parseable, otherwise the original
// string trimmed. Used for clawhub-style configs where the operator
// might declare just "clawhub.ai" without scheme.
func hostOfOrSelf(rawURL string) string {
	if h := hostOf(rawURL); h != "" {
		return h
	}
	return strings.TrimSpace(rawURL)
}

// uniqueHosts is a small order-preserving set used while
// accumulating hostnames per role. Order preservation matters for
// log output consistency, not for correctness — smokescreen's glob
// matching is order-insensitive.
type uniqueHosts struct {
	seen map[string]struct{}
	list []string
}

func (u *uniqueHosts) add(host string) {
	if u.seen == nil {
		u.seen = make(map[string]struct{})
	}
	if _, ok := u.seen[host]; ok {
		return
	}
	u.seen[host] = struct{}{}
	u.list = append(u.list, host)
}
