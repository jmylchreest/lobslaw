package node

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/binaries"
	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/modelsdev"
)

// wireEgress builds the smokescreen provider from current config +
// skill manifests and installs it as the active egress.Provider.
// Runs early in the assembly chain — every later stage that
// constructs an http.Client through egress.For(role) sees the
// production provider rather than the boot-time noop.
//
// CRITICAL invariant for the future Phase E.5 nftables work:
// smokescreen runs INSIDE THIS PROCESS in lobslaw's host network
// namespace. Subprocess sandbox netns rules MUST live in the
// SUBPROCESS's namespace ONLY — never installed in the host
// namespace, or lobslaw's own egress redirects back to itself in an
// infinite loop. The netns scoping is the property that makes
// "redirect outbound to smokescreen" safe.
//
// The provider runs on a goroutine started by NewSmokescreenProvider;
// its lifecycle is tied to the Node's shutdown via closePartial.
func (n *Node) wireEgress() error {
	rules := egress.Build(buildEgressInputs(n))
	prov, err := egress.NewSmokescreenProvider(egress.SmokescreenConfig{
		ACL:                rules,
		AllowPrivateRanges: n.cfg.Security.EgressAllowPrivateRanges,
		AllowRanges:        append(remoteAllowRanges(n), n.cfg.Security.EgressAllowRanges...),
		UpstreamProxy:      n.cfg.Security.EgressUpstreamProxy,
		UDSPath:            n.cfg.Security.EgressUDSPath,
		Logger:             n.log,
	})
	if err != nil {
		return err
	}
	n.egressProvider = prov
	egress.SetActiveProvider(prov)
	return nil
}

// refreshEgressACL regenerates the egress ACL from current config
// and live registries (skills, MCP servers, storage mounts) and
// applies it atomically via SmokescreenProvider.SetACL. Idempotent
// and safe to call from any goroutine.
//
// Trigger sources today: NodeService.Reload sections=["egress"]
// (operator-driven). Future: skill registry change hook fires this
// when a skill is added/removed; storage change hook fires it when
// a mount's network config changes.
func (n *Node) refreshEgressACL() error {
	if n.egressProvider == nil {
		return nil
	}
	rules := egress.Build(buildEgressInputs(n))
	n.egressProvider.SetACL(rules)
	return nil
}

// subprocessProxyURL returns the HTTPS_PROXY URL a skill or other
// spawned subprocess should use, encoded with the per-role identity
// so smokescreen sees the right ACL. Returns "" when no provider
// is wired (e.g. boot-time noop) — callers treat empty as "no
// proxy" and the subprocess egresses directly (only safe in tests).
//
// When networkIsolation is true and a UDS listener was configured,
// returns the UDS form (`unix:///<path>?role=<role>`) instead of
// TCP loopback — the netns-isolated subprocess can't reach loopback
// TCP but inherits the mount namespace. HTTP libraries that don't
// support unix:// URLs in HTTPS_PROXY (most non-Go ones) need a
// per-runtime adapter; document this in the skill manifest.
func (n *Node) subprocessProxyURL(role string, networkIsolation bool) string {
	if n.egressProvider == nil {
		return ""
	}
	if networkIsolation {
		if uds := n.egressProvider.UDSPath(); uds != "" {
			return "unix://" + uds + "?role=" + role
		}
		// netns-isolated subprocess with no UDS configured can't
		// reach the proxy. Returning empty makes HTTPS_PROXY unset;
		// the subprocess can only reach what's in its (typically
		// empty) netns. Operators wanting netns isolation MUST
		// configure security.egress_uds_path.
		n.log.Warn("subprocess: network_isolation requested but no UDS configured; egress will fail",
			"role", role)
		return ""
	}
	return n.egressProvider.SubprocessProxyURL(role)
}

// buildEgressInputs aggregates the live config + skill registry into
// the ACL builder's input shape. Called at boot and from
// refreshEgressACL when a skill or storage mount changes — which is
// what lets a skill installed after boot get its role.
func buildEgressInputs(n *Node) egress.ACLInputs {
	in := egress.ACLInputs{
		Providers:          n.cfg.Compute.Providers,
		Channels:           n.cfg.Gateway.Channels,
		ClawhubBaseURL:     n.cfg.Security.ClawhubBaseURL,
		ClawhubBinaryHosts: n.cfg.Security.ClawhubBinaryHosts,
		FetchURLAllowHosts: n.cfg.Security.FetchURLAllowHosts,
		Remotes:            n.cfg.Remotes,
	}
	// "binaries-install" egress role is pre-populated at boot with
	// the union of bootstrap-installer hosts (raw.githubusercontent.com,
	// astral.sh, etc.) and the runtime upstream hosts of every
	// Bootstrappable manager (formulae.brew.sh, ghcr.io, pypi.org,
	// ...). This means clawhub_install with bootstrap_managers=true
	// can reach the curl-sh installers + the post-bootstrap install
	// flow without operator config gymnastics.
	in.BinariesInstallHosts = binaries.DefaultInstallHosts()
	if len(n.cfg.Security.OAuth) > 0 {
		eps := make(map[string]egress.OAuthEndpoints, len(n.cfg.Security.OAuth))
		for name := range n.cfg.Security.OAuth {
			defaults := defaultOAuthProvider(name)
			raw := n.cfg.Security.OAuth[name]
			da := raw.DeviceAuthEndpoint
			if da == "" {
				da = defaults.DeviceAuthEndpoint
			}
			tok := raw.TokenEndpoint
			if tok == "" {
				tok = defaults.TokenEndpoint
			}
			eps[name] = egress.OAuthEndpoints{
				DeviceAuthEndpoint: da,
				TokenEndpoint:      tok,
			}
		}
		in.OAuthProviders = eps
	}
	// Mirrors the predicate applyModelsDevAutoCapabilities uses, so
	// the allowance exists exactly when the fetch happens.
	for _, prov := range n.cfg.Compute.Providers {
		if prov.AutoCapabilities || prov.AutoPricing {
			in.WantsModelDiscovery = true
			break
		}
	}
	in.ModelsDevURL = modelsdev.DefaultURL
	// Only when a download is actually configured.
	if n.cfg.Compute.Embeddings.Builtin() {
		in.EmbeddingModelURL = n.cfg.Compute.Embeddings.DownloadURL
	}

	in.WebSearchHosts = webSearchEgressHosts(n)

	in.CallbackHosts = callbackEgressHosts(n)
	in.MCPServerNetworks = mcpEgressHosts(n)
	in.SkillNetworks = skillNetworks(n)
	return in
}

// webSearchEgressHosts derives the web_search role's allowlist from
// the search backends config actually selected, and warns about the
// one deployment shape where a correct allowlist still fails.
//
// That shape is a self-hosted SearXNG, which is the whole point of the
// feature and lands on a private address every time. Smokescreen's IP
// filter denies RFC1918 no matter what the hostname ACL says, so
// without security.egress_allow_ranges the operator gets a proxy
// rejection that names neither the range nor the setting that fixes
// it. One line at boot costs nothing and saves that afternoon.
func webSearchEgressHosts(n *Node) []string {
	providers := resolvedSearchProviders(n.cfg.Compute)
	if len(providers) == 0 {
		return nil
	}
	privateOK := n.cfg.Security.EgressAllowPrivateRanges || len(n.cfg.Security.EgressAllowRanges) > 0
	hosts := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, p := range providers {
		endpoint := p.Endpoint
		switch normaliseSearchDriver(p.Driver) {
		case "", compute.DriverExa:
			endpoint = compute.ExaEffectiveEndpoint(endpoint)
		case compute.DriverKagi:
			endpoint = compute.KagiEffectiveEndpoint(endpoint)
		}
		host := egress.HostOf(endpoint)
		if host == "" {
			continue
		}
		// A chain whose backends share a host would otherwise repeat it
		// in the ACL and in the warning below.
		if _, dup := seen[host]; dup {
			continue
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
		if !privateOK && isPrivateHost(host) {
			n.log.Warn("egress: web_search provider is on a private address that smokescreen will refuse; "+
				"set security.egress_allow_ranges to the network it is on",
				"provider", p.Label, "host", host)
		}
	}
	return hosts
}

// isPrivateHost reports whether host is a literal private, loopback or
// link-local address. Names are not resolved: DNS at boot would be a
// new failure mode for a warning, and the container-name case
// ("searxng") is caught by its own check below.
func isPrivateHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	// A bare label with no dots is a container or compose service
	// name, which resolves onto the bridge network — private by
	// construction. A dotted name might be anything, so it is left
	// alone rather than guessed at.
	return !strings.Contains(host, ".")
}

// skillNetworks maps each skill that DECLARED a network onto the hosts
// it declared, so the ACL builder can give it a skill/<name> role —
// which is the role the invoker already sets on the subprocess.
//
// A skill that declares no `network:` is handled by
// [security] strict_skill_egress. Off (the default) it is omitted, so
// it keeps falling through to DefaultAllowedHosts — because the builder
// reads an empty host list as an explicit deny, and every skill written
// before this input was populated omits the field. Denying them by
// default would be a silent breaking change to a running deployment
// rather than a fix. On, it is mapped to nil and denied.
//
// Either way boot names them, so an operator can see what is
// unconfined rather than reading a permissive default as a working ACL.
func skillNetworks(n *Node) map[string][]string {
	out := map[string][]string{}
	if n.skillRegistry == nil {
		return out
	}
	strict := n.cfg.Security.StrictSkillEgress
	var undeclared []string
	for _, s := range n.skillRegistry.List() {
		if s == nil {
			continue
		}
		if hosts := s.Manifest.Network; len(hosts) > 0 {
			out[s.Name()] = append([]string(nil), hosts...)
			continue
		}
		undeclared = append(undeclared, s.Name())
		if strict {
			// nil, not absent: the builder registers the role with no
			// allowed hosts so smokescreen reports a deny naming the
			// skill, rather than the caller hitting the unknown-role
			// path and being told nothing useful.
			out[s.Name()] = nil
		}
	}
	if len(undeclared) > 0 {
		sort.Strings(undeclared)
		if strict {
			n.log.Warn("egress: skills with no declared network are DENIED egress "+
				"(security.strict_skill_egress is on); add a network: list to the manifest",
				"skills", undeclared)
		} else {
			n.log.Warn("egress: skills with no declared network reach any host the default ACL "+
				"allows; add a network: list to the manifest, then set "+
				"security.strict_skill_egress to enforce it",
				"skills", undeclared)
		}
	}
	return out
}

// callbackEgressHosts derives the gateway/callback role from the
// callback addresses operators bound in [[user]].channels, and warns
// about the deployment shape where a correct allowlist still fails.
//
// That shape is a callback aimed at the operator's own tooling, which
// lands on a private address most of the time and is exactly what this
// feature is for. Smokescreen's IP filter refuses RFC1918 whatever the
// hostname ACL says, so without security.egress_allow_ranges the
// operator gets a proxy rejection naming neither the range nor the
// setting that fixes it.
func callbackEgressHosts(n *Node) []string {
	privateOK := n.cfg.Security.EgressAllowPrivateRanges || len(n.cfg.Security.EgressAllowRanges) > 0
	var hosts []string
	seen := map[string]struct{}{}
	for _, u := range n.cfg.Users {
		for _, c := range u.Channels {
			if c.Type != gateway.ChannelCallback {
				continue
			}
			host := egress.HostOf(c.Address)
			if host == "" {
				n.log.Warn("egress: callback address has no host and will never be reachable",
					"user", u.ID, "address", c.Address)
				continue
			}
			if _, dup := seen[host]; dup {
				continue
			}
			seen[host] = struct{}{}
			hosts = append(hosts, host)
			if !privateOK && isPrivateHost(host) {
				n.log.Warn("egress: callback host is a private address that smokescreen will refuse; "+
					"set security.egress_allow_ranges to the network it is on",
					"user", u.ID, "host", host)
			}
		}
	}
	return hosts
}

// remoteAllowRanges turns each declared remote into a CIDR the proxy
// will admit.
//
// The role allowlist and the address check are separate layers in
// smokescreen: classifyAddr consults AllowRanges before it consults
// the RFC1918 blocklist, so an explicit range is what lets a private
// remote through WITHOUT egress_allow_private_ranges — which would
// open the whole local network rather than the one host declared.
//
// An operator-declared allow_cidr wins. Otherwise the host is
// resolved to a /32 (or /128), which is right until the address moves
// and is why declaring it is the better answer for anything on DHCP.
// A name that will not resolve at boot is skipped with a warning
// rather than failing the node: the remote may simply be down, and
// refusing to start over a host that is asleep is worse than a dial
// that fails later with a clear reason.
func remoteAllowRanges(n *Node) []string {
	out := make([]string, 0, len(n.cfg.Remotes))
	for _, r := range n.cfg.Remotes {
		if cidr := strings.TrimSpace(r.AllowCIDR); cidr != "" {
			out = append(out, cidr)
			continue
		}
		host := strings.TrimSpace(r.Host)
		if host == "" {
			continue
		}
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			n.log.Warn("egress: remote host did not resolve; its dials will be refused by the proxy "+
				"until it does or allow_cidr is set",
				"remote", r.Name, "host", host, "err", err)
			continue
		}
		for _, ip := range ips {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, fmt.Sprintf("%s/%d", ip.String(), bits))
		}
	}
	return out
}

// mcpEgressHosts derives each MCP server's allowlist.
//
// The field this fills existed, and was set to an empty map
// unconditionally. Nothing registered a role, so every request from
// an MCP server fell through to DefaultAllowedHosts — empty in
// production — and the effect was the worst of both: a server that
// honoured HTTPS_PROXY was denied every call, and one that ignored
// proxy env was unrestricted. The mechanism was built and switched
// off by one line.
//
// A REMOTE server's allowlist is its own URL host. Nothing else needs
// writing and there is no second list to drift, which is the rule
// [[remote]] already follows.
//
// A STDIO server's is whatever it declares in networks. Empty means
// it may reach nothing — the correct default for a subprocess whose
// outbound traffic nobody has thought about, and the one that makes
// the omission visible as a denial rather than as unbounded access.
func mcpEgressHosts(n *Node) map[string][]string {
	out := make(map[string][]string, len(n.cfg.MCP.Servers))
	for name, s := range n.cfg.MCP.Servers {
		if s.Disabled {
			continue
		}
		if strings.TrimSpace(s.URL) != "" {
			host := hostOf(s.URL)
			if host == "" {
				n.log.Warn("mcp: server url has no host; it will reach nothing",
					"server", name, "url", s.URL)
				out[name] = nil
				continue
			}
			// Said out loud, because a private address is the shape a
			// self-hosted MCP server takes every time and
			// smokescreen refuses RFC1918 whatever the hostname ACL
			// says. Same trap the web_search allowlist warns about.
			n.log.Info("mcp: remote server pinned to its own host",
				"server", name, "host", host)
			out[name] = []string{host}
			continue
		}
		out[name] = s.Networks
		if len(s.Networks) == 0 {
			n.log.Info("mcp: stdio server declares no networks; its outbound calls are denied",
				"server", name, "hint", "set networks = [\"host\"] under [mcp.servers.<name>]")
		}
	}
	return out
}

// hostOf returns the hostname of a URL, without the port.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
