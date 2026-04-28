package node

import (
	"github.com/jmylchreest/lobslaw/internal/binaries"
	"github.com/jmylchreest/lobslaw/internal/egress"
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
		AllowRanges:        n.cfg.Security.EgressAllowRanges,
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

// buildEgressInputs aggregates the live config + skill registry
// into the ACL builder's input shape. Called at boot and on every
// config hot-reload (Phase E.6 wires the reload trigger).
//
// Skill networks are TODO: the registry isn't populated this early
// in boot (Watch starts later). For v1 we register skills with
// permissive networks; Phase E.6 + skill registry's change hook
// will narrow them as manifests load.
func buildEgressInputs(n *Node) egress.ACLInputs {
	in := egress.ACLInputs{
		Providers:          n.cfg.Compute.Providers,
		Channels:           n.cfg.Gateway.Channels,
		ClawhubBaseURL:     n.cfg.Security.ClawhubBaseURL,
		ClawhubBinaryHosts: n.cfg.Security.ClawhubBinaryHosts,
		FetchURLAllowHosts: n.cfg.Security.FetchURLAllowHosts,
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
	// MCP servers + skill networks: rules emerge once subprocesses
	// spawn. Phase E.4 + E.6 fold them in.
	in.MCPServerNetworks = map[string][]string{}
	in.SkillNetworks = map[string][]string{}
	return in
}
