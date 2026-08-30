package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/internal/secrets"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
)

// dispatchDoctor handles `lobslaw doctor`. Returns true if handled.
func dispatchDoctor(args []string) bool {
	idx := findSubcmd(args, "doctor")
	if idx < 0 {
		return false
	}
	lobslawDoctor(args[idx+1:])
	return true
}

// doctorCheck is one diagnostic. Run returns a short pass/fail
// message; a non-nil problem means the check failed and a non-zero
// exit code should follow.
type doctorCheck struct {
	Name string
	Run  func() (detail string, problem error)
}

// doctorEnv is what every check needs to do its job. Checks are
// methods on it rather than closures in the table: as closures their
// branching all counted against lobslawDoctor, which is how that
// function reached complexity 47 while being nothing more than a list.
type doctorEnv struct {
	cfg     *config.Config
	cfgPath string
	offline bool
}

// lobslawDoctor runs every check and reports pass/fail. The checks
// themselves are methods on doctorEnv, so this stays a list.
func lobslawDoctor(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := fs.String("config", envOr("LOBSLAW_CONFIG", ""), "path to config.toml")
	offline := fs.Bool("offline", false, "skip network reachability checks")
	_ = fs.Parse(args)

	if *cfgPath == "" {
		exitWith("doctor: --config (or LOBSLAW_CONFIG) required")
	}

	// Config must load before anything else — every other check
	// depends on its values. Done eagerly so subsequent checks can
	// close over the parsed struct.
	cfg, err := config.Load(config.LoadOptions{Path: *cfgPath})
	if err != nil {
		fmt.Printf("FAIL  config parse: %v\n", err)
		os.Exit(1)
	}

	d := doctorEnv{cfg: cfg, cfgPath: *cfgPath, offline: *offline}

	checks := []doctorCheck{
		{Name: ".env readable + chmod 0600", Run: d.checkEnvFilePerms},
		{Name: "memory encryption key", Run: d.checkMemoryKey},
		{Name: "mTLS CA certificate", Run: d.checkCACert},
		{Name: "mTLS node cert + key", Run: d.checkNodeCert},
		{Name: "SOUL.md parses", Run: d.checkSoul},
		{Name: "audit.local path writable", Run: d.checkAuditWritable},
		{Name: "oauth providers configured", Run: d.checkOAuthProviders},
		{Name: "skill storage mounts declared", Run: d.checkSkillMounts},
		{Name: "identity aliases", Run: d.checkIdentityAliases},
		{Name: "[[user]] roles reachable", Run: d.checkUserRolesReachable},
		{Name: "operator role granted", Run: d.checkOperatorGrant},
		{Name: "secret providers", Run: d.checkSecretProviders},
		{Name: "egress fetch_url scope", Run: d.checkFetchScope},
		{Name: "LLM provider reachable", Run: d.checkLLMReachable},
	}

	var failures int
	for _, c := range checks {
		detail, err := c.Run()
		if err != nil {
			fmt.Printf("FAIL  %s: %v\n", c.Name, err)
			failures++
			continue
		}
		fmt.Printf("OK    %s: %s\n", c.Name, detail)
	}

	if failures > 0 {
		fmt.Fprintf(os.Stderr, "\n%d check(s) failed\n", failures)
		os.Exit(1)
	}
	fmt.Println("\nall checks passed")
}

func (d doctorEnv) checkEnvFilePerms() (string, error) {
	envFile := filepath.Join(filepath.Dir(d.cfgPath), ".env")
	fi, err := os.Stat(envFile)
	if err != nil {
		return "", fmt.Errorf("stat %q: %w", envFile, err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		return "", fmt.Errorf("%q has mode %o; want 0600 (contains secrets)", envFile, mode)
	}
	return envFile, nil
}

// checkSecretProviders builds every declared provider and resolves one
// reference through each.
//
// Constructing them is not enough: a missing binary, a locked vault and
// an expired session all build fine and fail at the first fetch, which
// on this node is during wiring — so an operator who ran doctor and saw
// OK would still watch the node refuse to boot. The probe is what makes
// the check mean something.
func (d doctorEnv) checkSecretProviders() (string, error) {
	if len(d.cfg.Secrets.Providers) == 0 {
		return "none declared (env: and file: always available)", nil
	}
	resolver, err := secrets.FromConfig(d.cfg.Secrets, secrets.DefaultRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return "", err
	}
	probes := d.secretRefsByScheme()
	ok := make([]string, 0, len(d.cfg.Secrets.Providers))
	for _, p := range d.cfg.Secrets.Providers {
		label := strings.ToLower(strings.TrimSpace(p.Label))
		ref, found := probes[label]
		if !found {
			// Declared and unused is odd but not wrong — an operator
			// may be mid-migration. Say so rather than inventing a
			// reference and reporting a failure they cannot act on.
			ok = append(ok, label+" (declared, no reference uses it yet)")
			continue
		}
		if _, err := resolver.Resolve(ref); err != nil {
			return "", fmt.Errorf("provider %q: %w", label, err)
		}
		ok = append(ok, label+" ✓")
	}
	return strings.Join(ok, ", "), nil
}

// secretRefsByScheme finds one real reference per scheme from the
// config, so the probe exercises a path the operator actually uses
// rather than one this function made up.
func (d doctorEnv) secretRefsByScheme() map[string]string {
	out := map[string]string{}
	note := func(ref string) {
		scheme, _, ok := strings.Cut(strings.TrimSpace(ref), ":")
		if !ok || scheme == "" {
			return
		}
		scheme = strings.ToLower(scheme)
		if _, seen := out[scheme]; !seen {
			out[scheme] = ref
		}
	}
	for _, p := range d.cfg.Compute.Providers {
		note(p.APIKeyRef)
	}
	note(d.cfg.Compute.Embeddings.APIKeyRef)
	for _, ch := range d.cfg.Gateway.Channels {
		note(ch.BotTokenRef)
		note(ch.AppTokenRef)
		note(ch.SecretTokenRef)
		note(ch.SharedSecretRef)
	}
	for _, srv := range d.cfg.MCP.Servers {
		for _, ref := range srv.SecretEnv {
			note(ref)
		}
	}
	// OAuth and the JWT secret are easy to forget and are exactly the
	// kind of reference an operator moves into a vault first, being the
	// longest-lived credentials on the node.
	for _, p := range d.cfg.Security.OAuth {
		note(p.ClientIDRef)
		note(p.ClientSecretRef)
	}
	note(d.cfg.Auth.JWTSecretRef)
	return out
}

func (d doctorEnv) checkMemoryKey() (string, error) {
	ref := d.cfg.Memory.Encryption.KeyRef
	if ref == "" {
		return "", fmt.Errorf("memory.encryption.key_ref is empty")
	}
	val, err := secrets.Bootstrap(ref)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", ref, err)
	}
	raw, err := base64.StdEncoding.DecodeString(val)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("%d bytes decoded; want 32", len(raw))
	}
	return "32-byte key resolved via " + ref, nil
}

func (d doctorEnv) checkCACert() (string, error) {
	path := d.cfg.Cluster.MTLS.CACert
	if path == "" {
		return "", fmt.Errorf("cluster.mtls.ca_cert not set")
	}
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	return path, nil
}

func (d doctorEnv) checkNodeCert() (string, error) {
	ca := d.cfg.Cluster.MTLS.CACert
	cert := d.cfg.Cluster.MTLS.NodeCert
	key := d.cfg.Cluster.MTLS.NodeKey
	if ca == "" || cert == "" || key == "" {
		return "", fmt.Errorf("cluster.mtls.{ca_cert,node_cert,node_key} must all be set")
	}
	// LoadNodeCreds parses the full bundle — a mangled
	// PEM, mismatched key, or unsigned cert fails here
	// with a descriptive error.
	if _, err := mtls.LoadNodeCreds(ca, cert, key); err != nil {
		return "", err
	}
	return cert, nil
}

func (d doctorEnv) checkSoul() (string, error) {
	path := d.cfg.Soul.Path
	if path == "" {
		return "default (no SoulPath set)", nil
	}
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	return path, nil
}

func (d doctorEnv) checkAuditWritable() (string, error) {
	if !d.cfg.Audit.Local.Enabled {
		return "disabled", nil
	}
	p := d.cfg.Audit.Local.Path
	if p == "" {
		return "", fmt.Errorf("audit.local.path is empty")
	}
	// Probe the parent directory with a temp create; a
	// read-only mount is a common first-run surprise.
	dir := filepath.Dir(p)
	tmp, err := os.CreateTemp(dir, ".lobslaw-doctor-*")
	if err != nil {
		return "", fmt.Errorf("write probe in %q: %w", dir, err)
	}
	_ = tmp.Close()
	_ = os.Remove(tmp.Name())
	return p, nil
}

func (d doctorEnv) checkOAuthProviders() (string, error) {
	if len(d.cfg.Security.OAuth) == 0 {
		return "none configured (oauth_start unavailable)", nil
	}
	known := map[string]bool{
		"google": true, "github": true, "microsoft": true, "gitlab": true,
	}
	for name, p := range d.cfg.Security.OAuth {
		if p.ClientIDRef == "" {
			return "", fmt.Errorf("oauth provider %q: client_id_ref is required", name)
		}
		if _, err := config.ResolveSecret(p.ClientIDRef); err != nil {
			return "", fmt.Errorf("oauth %q client_id_ref: %w", name, err)
		}
		if p.ClientSecretRef != "" {
			if _, err := config.ResolveSecret(p.ClientSecretRef); err != nil {
				return "", fmt.Errorf("oauth %q client_secret_ref: %w", name, err)
			}
		}
		if !known[name] && (p.DeviceAuthEndpoint == "" || p.TokenEndpoint == "") {
			return "", fmt.Errorf("oauth %q is not a built-in provider; declare device_auth_endpoint + token_endpoint explicitly", name)
		}
	}
	names := make([]string, 0, len(d.cfg.Security.OAuth))
	for name := range d.cfg.Security.OAuth {
		names = append(names, name)
	}
	return fmt.Sprintf("%d configured (%v)", len(names), names), nil
}

func (d doctorEnv) checkSkillMounts() (string, error) {
	if len(d.cfg.Storage.Mounts) == 0 {
		return "no [[storage.mounts]] (skills + clawhub install will fail)", nil
	}
	labels := make(map[string]bool, len(d.cfg.Storage.Mounts))
	for _, m := range d.cfg.Storage.Mounts {
		if m.Label == "" {
			return "", fmt.Errorf("storage mount has empty label")
		}
		if labels[m.Label] {
			return "", fmt.Errorf("duplicate storage label %q", m.Label)
		}
		labels[m.Label] = true
	}
	if d.cfg.Security.ClawhubBaseURL != "" {
		target := d.cfg.Security.ClawhubInstallMount
		if target == "" {
			target = config.DefaultSkillMountLabel
		}
		if !labels[target] {
			return "", fmt.Errorf("clawhub install mount %q not in [[storage.mounts]]", target)
		}
	}
	out := make([]string, 0, len(labels))
	for l := range labels {
		out = append(out, l)
	}
	return fmt.Sprintf("%d declared (%v)", len(out), out), nil
}

func (d doctorEnv) checkFetchScope() (string, error) {
	if len(d.cfg.Security.FetchURLAllowHosts) == 0 {
		return "permissive (any public host; tighten via [security].fetch_url_allow_hosts)", nil
	}
	return fmt.Sprintf("%d host(s) allowlisted", len(d.cfg.Security.FetchURLAllowHosts)), nil
}

func (d doctorEnv) checkLLMReachable() (string, error) {
	if d.offline {
		return "skipped (--offline)", nil
	}
	if len(d.cfg.Compute.Providers) == 0 {
		return "", fmt.Errorf("no [[compute.providers]] configured")
	}
	first := d.cfg.Compute.Providers[0]
	if first.Endpoint == "" {
		return "", fmt.Errorf("provider %q has empty endpoint", first.Label)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, first.Endpoint, nil)
	if err != nil {
		return "", err
	}
	// `lobslaw doctor` runs as a one-shot CLI command,
	// not inside a node process, so no smokescreen is
	// running. The egress factory's noop provider returns
	// a vanilla http.Client — which is what we want for
	// a connectivity probe (the whole point is to verify
	// the operator's endpoint is reachable from this host
	// before they boot the node, where smokescreen would
	// then enforce the ACL).
	resp, err := egress.For("doctor").HTTPClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("dial %q: %w", first.Endpoint, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	// Any HTTP response (even 401/404) proves the endpoint
	// resolves + TLS handshakes. Real auth happens at
	// request time; doctor only cares about reachability.
	return fmt.Sprintf("%s → HTTP %d", first.Endpoint, resp.StatusCode), nil
}

// checkIdentityAliases reports the resolved alias map.
//
// A mistyped alias key fails silently and open: the channel id simply
// resolves to itself, and that person quietly stops finding their own
// history from the other channel. Nothing errors, nothing logs — so
// the only way an operator learns is by noticing an absence. Printing
// the map is most of the value here; the failure cases below are the
// ones we can actually prove.
func (d doctorEnv) checkIdentityAliases() (string, error) {
	aliases := d.cfg.Identity.Aliases
	if len(aliases) == 0 {
		return "none declared (every channel id is its own principal)", nil
	}

	declared := map[string]bool{}
	for _, u := range d.cfg.Users {
		if u.ID != "" {
			declared[u.ID] = true
		}
	}

	// An alias pointing at an id no [[user]] declares is not fatal —
	// principals need not be declared — but it is the shape a typo
	// takes, so name it when there are [[user]] blocks to compare
	// against.
	var unknown []string
	if len(declared) > 0 {
		for from, to := range aliases {
			if !declared[to] {
				unknown = append(unknown, fmt.Sprintf("%q → %q", from, to))
			}
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		return "", fmt.Errorf("%d alias(es) point at an id no [[user]] declares: %s "+
			"(a typo here resolves the id to itself and silently splits that person's history)",
			len(unknown), strings.Join(unknown, ", "))
	}

	pairs := make([]string, 0, len(aliases))
	for from, to := range aliases {
		pairs = append(pairs, from+" → "+to)
	}
	sort.Strings(pairs)
	return fmt.Sprintf("%d alias(es): %s", len(pairs), strings.Join(pairs, ", ")), nil
}

// checkUserRolesReachable catches roles that can never apply.
//
// Role lookup resolves a channel id through [identity.aliases] and
// then matches [[user]].id. Telegram yields "tg-@alice" while
// [[user]].channels binds numeric addresses, so those do not join —
// without an alias entry, a [[user]] with roles is inert on every
// channel that has no JWT. The declaration looks correct and does
// nothing, which is exactly the failure doctor exists to surface.
func (d doctorEnv) checkUserRolesReachable() (string, error) {
	targets := map[string]bool{}
	for _, to := range d.cfg.Identity.Aliases {
		targets[to] = true
	}

	var withRoles, unreachable []string
	for _, u := range d.cfg.Users {
		if len(u.Roles) == 0 {
			continue
		}
		withRoles = append(withRoles, u.ID)
		if !targets[u.ID] {
			unreachable = append(unreachable, u.ID)
		}
	}
	if len(withRoles) == 0 {
		return "no [[user]] declares roles", nil
	}
	sort.Strings(unreachable)
	if len(unreachable) > 0 {
		return "", fmt.Errorf("%s declare(s) roles but no [identity.aliases] entry resolves to them: "+
			"the roles apply on channels carrying a JWT and nowhere else. Add an alias for that "+
			"person's channel id, or drop the roles line if REST is the only channel they use",
			strings.Join(unreachable, ", "))
	}
	sort.Strings(withRoles)
	return fmt.Sprintf("%s reachable via aliases", strings.Join(withRoles, ", ")), nil
}

// checkOperatorGrant pairs the operator role with the rule that makes
// it mean anything.
//
// Holding role:operator confers nothing on its own — cross-owner reads
// are a policy grant, deliberately, so they can be denied or scoped.
// The two halves are declared in different blocks, so either can exist
// without the other: a role nobody grants is inert, and a grant nobody
// holds is dead. Both are quiet, and both look like the feature is on.
func (d doctorEnv) checkOperatorGrant() (string, error) {
	const (
		role   = "operator"
		action = "memory:read:any"
	)

	var holders []string
	for _, u := range d.cfg.Users {
		for _, r := range u.Roles {
			if r == role {
				holders = append(holders, u.ID)
			}
		}
	}

	var granted bool
	for _, r := range d.cfg.Policy.Rules {
		if r.Subject == "role:"+role && r.Action == action && r.Effect == "allow" {
			granted = true
			break
		}
	}

	switch {
	case len(holders) == 0 && !granted:
		return "no operator role in use", nil
	case len(holders) > 0 && !granted:
		return "", fmt.Errorf("%s hold role:%s but no [[policy.rules]] entry allows %q — "+
			"the role grants nothing until one does",
			strings.Join(holders, ", "), role, action)
	case len(holders) == 0 && granted:
		return "", fmt.Errorf("a rule allows %q for role:%s but no [[user]] holds that role — "+
			"the grant is dead", action, role)
	default:
		sort.Strings(holders)
		return fmt.Sprintf("%s hold role:%s, granted %q", strings.Join(holders, ", "), role, action), nil
	}
}
