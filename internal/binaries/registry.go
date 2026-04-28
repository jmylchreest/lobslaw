package binaries

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
)

// Satisfier resolves "I need binary X on PATH" against a pool of
// install Managers. It does not hold a static catalogue — callers
// hand in install specs at request time (typically synthesized
// from a clawhub bundle's clawdbot.install array). The lookup-
// before-install path is the common case: if the binary's already
// on PATH (baked image, bind-mount, prior install) the call is a
// cheap no-op.
type Satisfier struct {
	managers map[string]Manager
	runner   ProcessRunner
	log      *slog.Logger
	prefix   string
}

// Config wires the Satisfier. HTTPClient powers the curl-sh manager
// (pass an egress-aware client for the "binaries-install" role).
// InstallPrefix is where user-mode managers (npm/cargo/uvx/pipx/
// go-install/curl-sh) write — typically /lobslaw/usr/local. System
// managers (apt/dnf/pacman/apk) ignore the prefix and write to
// system paths.
type Config struct {
	HTTPClient    *http.Client
	Runner        ProcessRunner
	Logger        *slog.Logger
	InstallPrefix string
}

// New builds a Satisfier with the standard manager pool.
func New(cfg Config) *Satisfier {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Runner == nil {
		cfg.Runner = ShellRunner{}
	}
	return &Satisfier{
		managers: managersWithPrefix(cfg.HTTPClient, cfg.InstallPrefix),
		runner:   cfg.Runner,
		log:      cfg.Logger,
		prefix:   cfg.InstallPrefix,
	}
}

func defaultManagers(client *http.Client) map[string]Manager {
	return managersWithPrefix(client, "")
}

func managersWithPrefix(client *http.Client, prefix string) map[string]Manager {
	out := map[string]Manager{
		"apt":        aptManager{},
		"brew":       brewManager{},
		"pacman":     pacmanManager{},
		"dnf":        dnfManager{},
		"apk":        apkManager{},
		"pipx":       pipxManager{prefix: prefix},
		"uvx":        uvxManager{prefix: prefix},
		"npm":        npmManager{prefix: prefix},
		"cargo":      cargoManager{prefix: prefix},
		"go-install": goInstallManager{prefix: prefix},
	}
	if client != nil {
		out["curl-sh"] = newCurlShManagerWithPrefix(client, prefix)
	}
	return out
}

// Available reports whether name resolves on PATH (the satisfier's
// install prefix's bin dir is searched first; system PATH after).
// True means "no install needed" — the binary is already present.
func (s *Satisfier) Available(name string) bool {
	_, err := lookPathWithPrefix(name, s.prefix)
	return err == nil
}

// SatisfyResult is what Satisfy returns on success.
type SatisfyResult struct {
	Name             string
	Manager          string
	AlreadyAvailable bool
}

// Satisfy ensures name is available on PATH. If the binary is
// already there, returns AlreadyAvailable=true with no work done.
// Otherwise picks the first install spec matching the host, validates
// it, and runs the manager. Returns an error if no spec matches the
// host, the matching manager isn't wired, the manager can't run on
// this host, or the install command fails.
func (s *Satisfier) Satisfy(ctx context.Context, name string, installs []InstallSpec) (SatisfyResult, error) {
	if name == "" {
		return SatisfyResult{}, errors.New("binaries: name required")
	}
	if s.Available(name) {
		return SatisfyResult{Name: name, AlreadyAvailable: true}, nil
	}
	spec, ok := pickSpec(installs)
	if !ok {
		return SatisfyResult{}, fmt.Errorf("binaries: no install spec matches this host for %q", name)
	}
	if err := spec.Validate(); err != nil {
		return SatisfyResult{}, fmt.Errorf("binaries: install spec for %q: %w", name, err)
	}
	mgr, ok := s.managers[spec.Manager]
	if !ok {
		return SatisfyResult{}, fmt.Errorf("binaries: manager %q for %q not wired (curl-sh requires HTTPClient)", spec.Manager, name)
	}
	if spec.Sudo && mgr.UserMode() && spec.Manager != "curl-sh" {
		return SatisfyResult{}, fmt.Errorf("binaries: install spec for %q: manager %q is user-mode; sudo:true is not meaningful", name, spec.Manager)
	}
	if !mgr.Available(ctx) {
		return SatisfyResult{}, fmt.Errorf("binaries: manager %q for %q not present on this host", spec.Manager, name)
	}
	if err := mgr.Install(ctx, spec, s.runner, s.log); err != nil {
		return SatisfyResult{}, err
	}
	if !s.Available(name) {
		return SatisfyResult{
			Name:    name,
			Manager: spec.Manager,
		}, fmt.Errorf("binaries: install command for %q exited zero but binary still not on PATH — verify the install spec actually places the binary under %s/bin or the system PATH", name, s.prefix)
	}
	return SatisfyResult{
		Name:    name,
		Manager: spec.Manager,
	}, nil
}

// HostsFor returns the union of hostnames the supplied install
// specs will reach. Used to populate the smokescreen
// "binaries-install" egress role at install-pipeline runtime.
func (s *Satisfier) HostsFor(installs []InstallSpec) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, spec := range installs {
		mgr, ok := s.managers[spec.Manager]
		if !ok {
			continue
		}
		for _, h := range mgr.Hosts(spec) {
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
	return out
}

func pickSpec(installs []InstallSpec) (InstallSpec, bool) {
	for _, spec := range installs {
		if spec.Match() {
			return spec, true
		}
	}
	return InstallSpec{}, false
}

// LookPath resolves a binary name against the supplied prefix's bin
// dir first, then the system PATH. Exported so callers (skill
// invoker, doctor) can do consistent lookup without holding a
// Satisfier.
func LookPath(name, prefix string) (string, error) {
	return lookPathWithPrefix(name, prefix)
}

func lookPathWithPrefix(name, prefix string) (string, error) {
	if prefix != "" {
		candidate := strings.TrimRight(prefix, "/") + "/bin/" + name
		if info, err := osStat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return exec.LookPath(name)
}
