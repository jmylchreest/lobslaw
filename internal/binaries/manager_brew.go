package binaries

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
)

type brewManager struct {
	httpClient *http.Client
	prefix     string
}

func (brewManager) Name() string   { return "brew" }
func (brewManager) UserMode() bool { return true }

func (brewManager) Hosts(_ InstallSpec) []string {
	return []string{
		"formulae.brew.sh",
		"github.com",
		"objects.githubusercontent.com",
		"ghcr.io",
		"pkg-containers.githubusercontent.com",
		"raw.githubusercontent.com",
	}
}

func (brewManager) Available(_ context.Context) bool {
	_, err := exec.LookPath("brew")
	return err == nil
}

func (m brewManager) Install(ctx context.Context, spec InstallSpec, runner ProcessRunner, log *slog.Logger) error {
	args := append([]string{"install", spec.Package}, spec.Args...)
	env := brewEnv(spec.Args, m.prefix)
	return runManagerCmdEnv(ctx, runner, log, "brew", false, args, env)
}

// BootstrapURL is informational — the actual bootstrap path skips
// install.sh and clones the brew repo directly because install.sh
// hardcodes /home/linuxbrew/.linuxbrew as the prefix in
// non-interactive mode.
func (m brewManager) BootstrapURL() string { return "https://github.com/Homebrew/brew" }

// Bootstrap installs Homebrew at the satisfier's install prefix
// (typically /lobslaw/usr/local) by cloning the upstream repo and
// symlinking the brew binary. Skips install.sh because it ignores
// HOMEBREW_PREFIX on Linux non-interactive runs.
//
// Trade-off: brew's pre-built bottles are linker-pinned to
// /home/linuxbrew/.linuxbrew (or /usr/local on intel mac, etc.).
// Installing at a non-default prefix forces source builds for any
// formula whose bottle doesn't match. For Go-only formulae this is
// fast; heavy native-dep formulae (numpy, ffmpeg) compile slowly.
// Operators wanting bottles use a different deployment strategy
// (volume-mount /home/linuxbrew, or a separate brew-tools sidecar).
func (m brewManager) Bootstrap(ctx context.Context, satisfier *Satisfier) error {
	prefix := satisfier.prefix
	if prefix == "" {
		return errors.New("binaries: brew bootstrap requires Satisfier prefix (e.g. /lobslaw/usr/local)")
	}

	repoDir := prefix + "/Homebrew"
	if _, err := os.Stat(repoDir); err == nil {
		satisfier.log.Info("binaries: brew bootstrap: repo already present", "path", repoDir)
	} else {
		out, err := satisfier.runner.Run(ctx, "git", []string{
			"clone", "--depth=1",
			"https://github.com/Homebrew/brew",
			repoDir,
		}, nil)
		if err != nil {
			return fmt.Errorf("brew bootstrap: git clone: %w (output: %s)", err, truncate(out, 512))
		}
	}

	binDir := prefix + "/bin"
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("brew bootstrap: mkdir bin: %w", err)
	}
	linkPath := binDir + "/brew"
	target := repoDir + "/bin/brew"
	if existing, err := os.Readlink(linkPath); err == nil && existing == target {
		// Already linked.
	} else {
		_ = os.Remove(linkPath)
		if err := os.Symlink(target, linkPath); err != nil {
			return fmt.Errorf("brew bootstrap: symlink %s → %s: %w", linkPath, target, err)
		}
	}

	satisfier.log.Info("binaries: brew bootstrap ok", "prefix", prefix, "repo", repoDir, "binary", linkPath)
	if !m.Available(ctx) {
		return fmt.Errorf("binaries: brew bootstrap completed but %s not on PATH — verify $PATH includes %s", linkPath, binDir)
	}
	return nil
}

// brewEnv returns the env brew expects when running install/upgrade
// commands at a non-default prefix. HOMEBREW_NO_AUTO_UPDATE keeps
// each install fast; HOMEBREW_NO_ANALYTICS suppresses anonymous
// telemetry; HOMEBREW_NO_INSTALL_FROM_API forces use of the cloned
// tap for formula discovery (the API endpoint serves bottle
// metadata that doesn't apply to our prefix).
func brewEnv(_ []string, prefix string) []string {
	env := append([]string(nil), os.Environ()...)
	env = append(env,
		"HOMEBREW_NO_AUTO_UPDATE=1",
		"HOMEBREW_NO_ANALYTICS=1",
		"HOMEBREW_NO_INSTALL_FROM_API=1",
	)
	if prefix != "" {
		env = append(env, "HOMEBREW_PREFIX="+prefix)
	}
	return env
}
