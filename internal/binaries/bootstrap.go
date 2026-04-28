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

// hostsForBootstrap returns the hostnames the bootstrap recipes
// reach. Used by the egress builder to pre-allowlist these hosts in
// the "binaries-install" smokescreen role when the operator has
// opted into bootstrap (via [security] clawhub_bootstrap_managers).
func hostsForBootstrap() []string {
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
	if satisfier.prefix != "" {
		env = append(env, "HOMEBREW_PREFIX="+satisfier.prefix)
		env = append(env, "BUN_INSTALL="+satisfier.prefix)
		env = append(env, "CARGO_HOME="+satisfier.prefix+"/cargo")
	}

	out, err := satisfier.runner.Run(ctx, "/bin/sh", []string{tmp.Name()}, env)
	if err != nil {
		return fmt.Errorf("binaries: bootstrap %q sh: %w (output: %s)", mgrName, err, truncate(out, 512))
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
