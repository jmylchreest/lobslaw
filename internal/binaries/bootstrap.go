package binaries

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// bootstrapRecipe is one well-known manager's official curl-sh
// installer. URL is canonical; the manager's name on PATH is the
// key. checksumPrefix lets operators-with-strict-supply-chain pin
// to a specific upstream-published SHA — empty means "no pin"
// (operator opted in via the bootstrap_managers flag, accepting
// upstream's good faith).
type bootstrapRecipe struct {
	URL            string
	ChecksumPrefix string // "sha256:..."; empty = no pin
	Env            map[string]string
}

// knownBootstraps lists managers we know how to install. To extend,
// add an entry here AND wire BootstrapURL/Bootstrap on the Manager
// implementation in manager_*.go. apt/dnf/pacman/apk are absent
// on purpose — they're OS-level package managers, not curl-sh
// installable.
var knownBootstraps = map[string]bootstrapRecipe{
	"brew": {
		URL: "https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh",
		Env: map[string]string{
			"NONINTERACTIVE": "1",
			"CI":             "1",
		},
	},
	"uvx": {
		URL: "https://astral.sh/uv/install.sh",
	},
}

// DefaultInstallHosts returns the union of (a) bootstrap installer
// host names and (b) runtime upstream hosts of every Bootstrappable
// manager. The egress builder uses this as the boot-time allowlist
// for the "binaries-install" smokescreen role so that:
//   - The bootstrap fetch (e.g. raw.githubusercontent.com for brew)
//     succeeds without an additional operator config step.
//   - The bootstrapped manager's own installs (e.g. brew pulling
//     formulae from formulae.brew.sh + ghcr.io) succeed without a
//     follow-up config + reload.
// Operators with stricter supply-chain requirements override the
// allowlist via [security] binaries_install_hosts (future work).
func DefaultInstallHosts() []string {
	seen := make(map[string]struct{})
	var out []string
	addAll := func(hosts []string) {
		for _, h := range hosts {
			if h == "" {
				continue
			}
			if _, dup := seen[h]; dup {
				continue
			}
			seen[h] = struct{}{}
			out = append(out, h)
		}
	}
	addAll(HostsForBootstrap())
	for _, mgr := range defaultManagers(nil) {
		if _, ok := mgr.(Bootstrappable); ok {
			addAll(mgr.Hosts(InstallSpec{}))
		}
	}
	return out
}

// HostsForBootstrap returns the hostnames the bootstrap recipes
// reach (just the curl-sh installer URLs). Subset of
// DefaultInstallHosts.
func HostsForBootstrap() []string {
	seen := make(map[string]struct{})
	var out []string
	for _, r := range knownBootstraps {
		u, err := url.Parse(r.URL)
		if err != nil || u.Hostname() == "" {
			continue
		}
		h := u.Hostname()
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	return out
}

// runBootstrap downloads the recipe's installer, optionally checks
// SHA-256, runs it via /bin/sh with the recipe's env, and verifies
// the manager is now Available.
func runBootstrap(ctx context.Context, satisfier *Satisfier, mgrName string, client *http.Client) error {
	recipe, ok := knownBootstraps[mgrName]
	if !ok {
		return fmt.Errorf("binaries: no bootstrap recipe for %q", mgrName)
	}
	if client == nil {
		return fmt.Errorf("binaries: bootstrap %q requires HTTPClient on the satisfier", mgrName)
	}
	body, err := fetchBootstrapBody(ctx, client, recipe.URL)
	if err != nil {
		return fmt.Errorf("binaries: bootstrap %q fetch: %w", mgrName, err)
	}
	if recipe.ChecksumPrefix != "" {
		got := sha256.Sum256(body)
		expected := strings.TrimPrefix(recipe.ChecksumPrefix, "sha256:")
		if hex.EncodeToString(got[:]) != expected {
			return fmt.Errorf("binaries: bootstrap %q checksum mismatch", mgrName)
		}
	}

	tmp, err := os.CreateTemp("", "lobslaw-bootstrap-*.sh")
	if err != nil {
		return fmt.Errorf("binaries: bootstrap %q tempfile: %w", mgrName, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o700); err != nil {
		return err
	}

	env := append([]string(nil), os.Environ()...)
	for k, v := range recipe.Env {
		env = append(env, k+"="+v)
	}
	// Per-manager prefix overrides:
	// - brew: uses its hardcoded /home/linuxbrew/.linuxbrew default
	//   in non-interactive mode regardless of HOMEBREW_PREFIX. The
	//   Dockerfile pre-creates that path chown'd to nonroot so the
	//   install succeeds without sudo.
	// - bun, cargo, uv: respect their respective prefix env vars.
	if satisfier.prefix != "" && mgrName != "brew" {
		env = append(env, "BUN_INSTALL="+satisfier.prefix)
		env = append(env, "CARGO_HOME="+satisfier.prefix+"/cargo")
	}

	// brew/uv/bun installers all start with #!/bin/bash and use
	// bash-only features (arrays, [[ ]], readarray, etc.). When we
	// invoke them as `/bin/sh script.sh` the shebang is ignored and
	// busybox ash runs the script — which detects the non-bash
	// interpreter and aborts with "Bash is required to interpret
	// this script." Use bash directly when present, fall back to
	// /bin/sh for installers that genuinely need only POSIX.
	interp := "/bin/bash"
	if _, err := os.Stat(interp); err != nil {
		interp = "/bin/sh"
	}
	out, err := satisfier.runner.Run(ctx, interp, []string{tmp.Name()}, env)
	if err != nil {
		return fmt.Errorf("binaries: bootstrap %q %s: %w (output: %s)", mgrName, interp, err, truncate(out, 512))
	}
	satisfier.log.Info("binaries: bootstrap ok", "manager", mgrName, "url", recipe.URL, "output_head", truncate(out, 256))

	target, ok := satisfier.managers[mgrName]
	if !ok {
		return fmt.Errorf("binaries: bootstrap %q: manager not registered", mgrName)
	}
	if !target.Available(ctx) {
		return fmt.Errorf("binaries: bootstrap %q ran but manager still not on PATH (prefix=%s) — check the installer's script for prefix-handling", mgrName, satisfier.prefix)
	}
	return nil
}

// fetchBootstrapBody downloads a bootstrap installer with a 10MiB
// cap. Returns the body bytes for SHA verification + execution.
func fetchBootstrapBody(ctx context.Context, client *http.Client, urlStr string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	const maxScript = 10 << 20
	return io.ReadAll(io.LimitReader(resp.Body, maxScript+1))
}

// _ keeps slog imported for the runBootstrap log call when
// satisfier.log gets refactored.
var _ = slog.New
