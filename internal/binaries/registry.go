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
		"brew":       brewManager{httpClient: client, prefix: prefix},
		"pacman":     pacmanManager{},
		"dnf":        dnfManager{},
		"apk":        apkManager{},
		"pipx":       pipxManager{prefix: prefix},
		"uvx":        uvxManager{prefix: prefix, httpClient: client},
		"npm":        npmManager{prefix: prefix},
		"cargo":      cargoManager{prefix: prefix},
		"go-install": goInstallManager{prefix: prefix},
		// gh-release + curl-sh are always registered so DefaultInstallHosts
		// can enumerate them at boot for the binaries-install egress role.
		// When client is nil, their Available() returns false and Install
		// surfaces a clear "HTTPClient required" error rather than panicking.
		"gh-release": ghReleaseManager{prefix: prefix, httpClient: client},
	}
	if client != nil {
		out["curl-sh"] = newCurlShManagerWithPrefix(client, prefix)
	}
	return out
}

// SatisfyOptions controls per-call Satisfier behaviour. Default-zero
// is the strict path: missing managers are reported as errors and
// already-available binaries are short-circuited.
type SatisfyOptions struct {
	// BootstrapMissingManagers, when true, runs a manager's
	// official curl-sh installer before retrying the spec. Used by
	// clawhub_install / binary_install when the operator/agent
	// explicitly opts in.
	BootstrapMissingManagers bool

	// Force bypasses the Available() short-circuit, reinstalling
	// even when the binary is already on PATH. Used by
	// binary_install force=true and by version-mismatch detection
	// at boot.
	Force bool
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
// Otherwise walks the install specs that match the host OS/arch
// (in declared order — bundle author's preference is honoured) and
// picks the first one whose Manager is locally available. Specs
// with manager-not-installed get diagnosed and skipped, not aborted
// on; an unavailable brew on a Linux box just falls through to the
// next declared method (apt, etc.).
//
// Returns an error only when none of the OS-matching specs have a
// usable manager. The error includes what was declared vs. what's
// installed so the operator can diagnose without log-diving.
func (s *Satisfier) Satisfy(ctx context.Context, name string, installs []InstallSpec) (SatisfyResult, error) {
	return s.SatisfyOpts(ctx, name, installs, SatisfyOptions{})
}

// SatisfyOpts is Satisfy with per-call options. When
// BootstrapMissingManagers is true, a missing-but-Bootstrappable
// manager (currently brew, uvx) gets bootstrapped via its official
// curl-sh installer before we retry the spec.
func (s *Satisfier) SatisfyOpts(ctx context.Context, name string, installs []InstallSpec, opts SatisfyOptions) (SatisfyResult, error) {
	if name == "" {
		return SatisfyResult{}, errors.New("binaries: name required")
	}
	if !opts.Force && s.Available(name) {
		return SatisfyResult{Name: name, AlreadyAvailable: true}, nil
	}

	osMatches := pickAllMatching(installs)
	if len(osMatches) == 0 {
		return SatisfyResult{}, fmt.Errorf("binaries: no install spec matches this host for %q", name)
	}

	var (
		triedManagers   []string
		skipReasons     []string
		bootstrappable  []string
	)
	for _, spec := range osMatches {
		if err := spec.Validate(); err != nil {
			skipReasons = append(skipReasons, fmt.Sprintf("%s (validation: %v)", spec.Manager, err))
			continue
		}
		mgr, ok := s.managers[spec.Manager]
		if !ok {
			skipReasons = append(skipReasons, fmt.Sprintf("%s (not wired; curl-sh needs HTTPClient)", spec.Manager))
			continue
		}
		if spec.Sudo && mgr.UserMode() && spec.Manager != "curl-sh" {
			skipReasons = append(skipReasons, fmt.Sprintf("%s (user-mode + sudo:true is invalid)", spec.Manager))
			continue
		}
		if !mgr.Available(ctx) {
			triedManagers = append(triedManagers, spec.Manager)
			if bs, isBs := mgr.(Bootstrappable); isBs {
				bootstrappable = append(bootstrappable, spec.Manager)
				if opts.BootstrapMissingManagers {
					s.log.Info("binaries: bootstrap missing manager", "manager", spec.Manager, "url", bs.BootstrapURL())
					if err := bs.Bootstrap(ctx, s); err != nil {
						skipReasons = append(skipReasons, fmt.Sprintf("%s (bootstrap failed: %v)", spec.Manager, err))
						continue
					}
				} else {
					skipReasons = append(skipReasons, fmt.Sprintf("%s (not installed; can be bootstrapped from %s — pass bootstrap_managers=true to opt in)", spec.Manager, bs.BootstrapURL()))
					continue
				}
			} else {
				skipReasons = append(skipReasons, fmt.Sprintf("%s (not installed on this host)", spec.Manager))
				continue
			}
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

	available := s.availableManagerNames(ctx)
	hint := ""
	if len(bootstrappable) > 0 && !opts.BootstrapMissingManagers {
		hint = fmt.Sprintf(" — to attempt auto-bootstrap, retry with bootstrap_managers=true (would install: %v)", bootstrappable)
	}
	return SatisfyResult{}, fmt.Errorf(
		"binaries: %q declared installable via %v but none are usable on this host (skipped: %v; available managers: %v)%s",
		name, triedManagers, skipReasons, available, hint,
	)
}

func (s *Satisfier) availableManagerNames(ctx context.Context) []string {
	var out []string
	for name, mgr := range s.managers {
		if mgr.Available(ctx) {
			out = append(out, name)
		}
	}
	return out
}

func pickAllMatching(installs []InstallSpec) []InstallSpec {
	var out []InstallSpec
	for _, spec := range installs {
		if spec.Match() {
			out = append(out, spec)
		}
	}
	return out
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
