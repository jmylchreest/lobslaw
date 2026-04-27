package node

import (
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
		Logger:             n.log,
	})
	if err != nil {
		return err
	}
	n.egressProvider = prov
	egress.SetActiveProvider(prov)
	return nil
}

// subprocessProxyURL returns the HTTPS_PROXY URL a skill or other
// spawned subprocess should use, encoded with the per-role identity
// so smokescreen sees the right ACL. Returns "" when no provider
// is wired (e.g. boot-time noop) — callers treat empty as "no
// proxy" and the subprocess egresses directly (only safe in tests).
func (n *Node) subprocessProxyURL(role string) string {
	if n.egressProvider == nil {
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
	// MCP servers + skill networks: rules emerge once subprocesses
	// spawn. Phase E.4 + E.6 fold them in.
	in.MCPServerNetworks = map[string][]string{}
	in.SkillNetworks = map[string][]string{}
	return in
}
