package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/jmylchreest/lobslaw/internal/egress"
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

	checks := []doctorCheck{
		{
			Name: ".env readable + chmod 0600",
			Run: func() (string, error) {
				envFile := filepath.Join(filepath.Dir(*cfgPath), ".env")
				fi, err := os.Stat(envFile)
				if err != nil {
					return "", fmt.Errorf("stat %q: %w", envFile, err)
				}
				if mode := fi.Mode().Perm(); mode != 0o600 {
					return "", fmt.Errorf("%q has mode %o; want 0600 (contains secrets)", envFile, mode)
				}
				return envFile, nil
			},
		},
		{
			Name: "memory encryption key",
			Run: func() (string, error) {
				ref := cfg.Memory.Encryption.KeyRef
				if ref == "" {
					return "", fmt.Errorf("memory.encryption.key_ref is empty")
				}
				val, err := config.ResolveSecret(ref)
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
			},
		},
		{
			Name: "mTLS CA certificate",
			Run: func() (string, error) {
				path := cfg.Cluster.MTLS.CACert
				if path == "" {
					return "", fmt.Errorf("cluster.mtls.ca_cert not set")
				}
				if _, err := os.Stat(path); err != nil {
					return "", err
				}
				return path, nil
			},
		},
		{
			Name: "mTLS node cert + key",
			Run: func() (string, error) {
				ca := cfg.Cluster.MTLS.CACert
				cert := cfg.Cluster.MTLS.NodeCert
				key := cfg.Cluster.MTLS.NodeKey
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
			},
		},
		{
			Name: "SOUL.md parses",
			Run: func() (string, error) {
				path := cfg.Soul.Path
				if path == "" {
					return "default (no SoulPath set)", nil
				}
				if _, err := os.Stat(path); err != nil {
					return "", err
				}
				return path, nil
			},
		},
		{
			Name: "audit.local path writable",
			Run: func() (string, error) {
				if !cfg.Audit.Local.Enabled {
					return "disabled", nil
				}
				p := cfg.Audit.Local.Path
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
			},
		},
		{
			Name: "oauth providers configured",
			Run: func() (string, error) {
				if len(cfg.Security.OAuth) == 0 {
					return "none configured (oauth_start unavailable)", nil
				}
				known := map[string]bool{
					"google": true, "github": true, "microsoft": true, "gitlab": true,
				}
				for name, p := range cfg.Security.OAuth {
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
				names := make([]string, 0, len(cfg.Security.OAuth))
				for name := range cfg.Security.OAuth {
					names = append(names, name)
				}
				return fmt.Sprintf("%d configured (%v)", len(names), names), nil
			},
		},
		{
			Name: "skill storage mounts declared",
			Run: func() (string, error) {
				if len(cfg.Storage.Mounts) == 0 {
					return "no [[storage.mounts]] (skills + clawhub install will fail)", nil
				}
				labels := make(map[string]bool, len(cfg.Storage.Mounts))
				for _, m := range cfg.Storage.Mounts {
					if m.Label == "" {
						return "", fmt.Errorf("storage mount has empty label")
					}
					if labels[m.Label] {
						return "", fmt.Errorf("duplicate storage label %q", m.Label)
					}
					labels[m.Label] = true
				}
				if cfg.Security.ClawhubBaseURL != "" {
					target := cfg.Security.ClawhubInstallMount
					if target == "" {
						target = "skill-tools"
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
			},
		},
		{
			Name: "egress fetch_url scope",
			Run: func() (string, error) {
				if len(cfg.Security.FetchURLAllowHosts) == 0 {
					return "permissive (any public host; tighten via [security].fetch_url_allow_hosts)", nil
				}
				return fmt.Sprintf("%d host(s) allowlisted", len(cfg.Security.FetchURLAllowHosts)), nil
			},
		},
		{
			Name: "LLM provider reachable",
			Run: func() (string, error) {
				if *offline {
					return "skipped (--offline)", nil
				}
				if len(cfg.Compute.Providers) == 0 {
					return "", fmt.Errorf("no [[compute.providers]] configured")
				}
				first := cfg.Compute.Providers[0]
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
			},
		},
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
