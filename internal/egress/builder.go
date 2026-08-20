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

	// WantsModelDiscovery is true when any provider opted into
	// auto_capabilities. The role is only created then: a node that
	// never fetches the catalog should not be allowed to.
	WantsModelDiscovery bool
	// ModelsDevURL is the catalog endpoint, so a private mirror gets
	// the allowance instead of the public host.
	ModelsDevURL string

	// EmbeddingModelURL is where a builtin embedding model is fetched
	// from, when one is configured with a download_url. Empty means
	// nothing is fetched and no allowance is granted.
	EmbeddingModelURL string

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

	// models.dev capability catalog. The fetcher calls
	// egress.For("modelsdev"), and without a matching role here the
	// proxy refused it — so auto_capabilities logged
	//
	//	modelsdev: fetch failed; ... Request rejected by proxy
	//
	// and every opted-in provider silently fell back to whatever the
	// operator had hand-declared. The feature was wired end to end
	// except for the one line that let it out of the box.
	//
	// Gated on somebody having asked for discovery, so a node that
	// never fetches the catalog carries no allowance for it.
	if in.WantsModelDiscovery {
		rules.Roles["modelsdev"] = []string{hostOfOrSelf(in.ModelsDevURL)}
	}

	// A builtin embedding model is DOWNLOADED, which is egress, and a
	// distinct kind from calling an embedding API: the existing
	// "embedding" role carries the LLM provider hosts, which is the
	// wrong allowance entirely — a model comes from a mirror, not from
	// the vendor you send prompts to.
	//
	// Same shape as modelsdev above, and for the same reason it is
	// commented there: the feature can be wired end to end and still
	// fail on the one line that lets it out of the box. This one was.
	// Granted only when a download_url is set, so a node that ships
	// its model on disk carries no allowance for fetching one.
	if in.EmbeddingModelURL != "" {
		rules.Roles["embedding-model"] = modelDownloadHosts(in.EmbeddingModelURL)
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

// modelDownloadHosts is the host a model is fetched from, plus the
// hosts it REDIRECTS to.
//
// Allowing only the configured host is the obvious implementation and
// it does not work: github.com/<o>/<r>/releases/download/... answers
// 302 to a signed CDN URL on another host entirely, so the fetch dies
// with "Request rejected by proxy" on a policy that looks correct.
//
// Found by pointing a node at our own release mirror and watching it
// fail to boot. It is the third time this shape has bitten — see the
// modelsdev role above, whose comment says the feature can be wired end
// to end except for the one line that lets it out of the box.
//
// Listed rather than following redirects blindly: an allowance that
// widened to wherever a server pointed would not be an allowance.
func modelDownloadHosts(rawURL string) []string {
	host := hostOfOrSelf(rawURL)
	hosts := []string{host}
	if host == "github.com" || strings.HasSuffix(host, ".github.com") {
		hosts = append(hosts,
			// Where release assets are actually served from. Both are
			// live: the first is current, the second still answers for
			// older assets.
			"release-assets.githubusercontent.com",
			"objects.githubusercontent.com",
		)
	}
	return hosts
}
