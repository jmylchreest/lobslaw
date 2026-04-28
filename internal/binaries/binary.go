package binaries

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
)

// Binary describes one host binary the install pipeline may need to
// satisfy. Synthesized from a clawhub bundle's clawdbot.requires +
// install array; not operator config.
type Binary struct {
	// Name is the binary's PATH-resolvable name (e.g. "gh", "uvx").
	Name string

	// Description is free-text; carried through for diagnostics.
	Description string

	// Detect is an optional shell command that returns 0 iff the
	// binary is already installed. The Satisfier prefers PATH lookup
	// (LookPath) for "is it available?" and uses Detect only when an
	// install action wants a richer post-install verification.
	Detect string

	// Install enumerates per-OS install specs. Satisfier picks the
	// first spec whose OS (and Arch, if set) matches the host.
	Install []InstallSpec
}

// InstallSpec is one platform's install recipe.
type InstallSpec struct {
	// OS is the GOOS value: "linux", "darwin", "windows". Required.
	OS string

	// Arch narrows by GOARCH ("amd64", "arm64"). Empty matches any.
	Arch string

	// Distro narrows linux further: "debian", "ubuntu", "arch",
	// "fedora", "alpine". Empty matches any linux.
	Distro string

	// Manager is the install manager: "apt", "brew", "pacman",
	// "dnf", "apk", "pipx", "uvx", "npm", "cargo", "go-install",
	// "curl-sh".
	Manager string

	// Package is the package name as the manager understands it
	// (e.g. "github-cli" for pacman, "gh" for brew). Required for
	// every manager except curl-sh.
	Package string

	// Repo is an optional repository identifier. For apt this is
	// a "deb https://... main" line plus a key URL; for pacman an
	// AUR helper hint. Pre-configured operator setup is preferred —
	// the manager doesn't manage repo registration itself.
	Repo string

	// URL is the install-script URL for curl-sh. Required when
	// Manager == "curl-sh".
	URL string

	// Checksum is the expected SHA-256 of the URL body for curl-sh.
	// Format "sha256:<hex>". Required when Manager == "curl-sh"
	// (curl|bash without checksum is rejected).
	Checksum string

	// Sudo declares whether the install command needs sudo. The
	// runtime refuses to elevate; if Sudo is true, the operator
	// must have configured passwordless sudo for the manager
	// command, OR be running inside a container where the lobslaw
	// process is already root.
	Sudo bool

	// Args are extra arguments to pass to the manager (e.g.
	// ["--global"] for npm). Quoted as-is.
	Args []string
}

// Match reports whether this spec applies to the running host.
func (s InstallSpec) Match() bool {
	if s.OS != "" && s.OS != runtime.GOOS {
		return false
	}
	if s.Arch != "" && s.Arch != runtime.GOARCH {
		return false
	}
	if s.Distro != "" && !matchDistro(s.Distro) {
		return false
	}
	return true
}

// Validate reports config errors before runtime.
func (b Binary) Validate() error {
	if strings.TrimSpace(b.Name) == "" {
		return errors.New("binary: name required")
	}
	if !validNamePattern(b.Name) {
		return fmt.Errorf("binary %q: name must be lowercase alphanumeric + dash + dot", b.Name)
	}
	if len(b.Install) == 0 {
		return fmt.Errorf("binary %q: at least one install spec required", b.Name)
	}
	for i, spec := range b.Install {
		if err := spec.Validate(); err != nil {
			return fmt.Errorf("binary %q install[%d]: %w", b.Name, i, err)
		}
	}
	return nil
}

// Validate reports config errors for one InstallSpec.
func (s InstallSpec) Validate() error {
	if s.OS == "" {
		return errors.New("os required")
	}
	if s.Manager == "" {
		return errors.New("manager required")
	}
	known := map[string]bool{
		"apt": true, "brew": true, "pacman": true, "dnf": true,
		"apk": true, "pipx": true, "uvx": true, "npm": true,
		"cargo": true, "go-install": true, "curl-sh": true,
	}
	if !known[s.Manager] {
		return fmt.Errorf("unknown manager %q", s.Manager)
	}
	switch s.Manager {
	case "curl-sh":
		if s.URL == "" {
			return errors.New("curl-sh requires url")
		}
		if !strings.HasPrefix(s.Checksum, "sha256:") || len(s.Checksum) != len("sha256:")+64 {
			return errors.New("curl-sh requires checksum (sha256:<64hex>)")
		}
	default:
		if s.Package == "" {
			return fmt.Errorf("manager %q requires package", s.Manager)
		}
	}
	return nil
}

// validNamePattern is permissive enough for tool names (gh, gcloud,
// docker-compose, gh.cli) but rules out shell-special characters
// since the name flows into the policy resource string.
func validNamePattern(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') ||
			r == '-' || r == '.' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}
