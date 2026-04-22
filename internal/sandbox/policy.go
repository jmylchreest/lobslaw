package sandbox

import (
	"errors"
	"fmt"
)

// Policy describes the sandboxing to apply to a tool subprocess.
// The zero-value Policy is "no sandbox" — callers explicitly opt in
// by populating fields.
//
// Not every field is enforced by the current Apply implementation.
// See internal/sandbox/README (and DEFERRED.md) for the matrix of
// what's wired now vs. what's deferred — the struct reflects the
// full design so config files stay forward-compatible.
type Policy struct {
	// AllowedPaths bind-mounted read-write into the sandbox. Anything
	// else on the host filesystem is invisible. Paths must be
	// absolute; pre-canonicalised by Validate.
	AllowedPaths []string

	// ReadOnlyPaths subset of AllowedPaths bind-mounted read-only.
	// Entries here MUST also appear in AllowedPaths. Verified by
	// Validate.
	ReadOnlyPaths []string

	// NetworkAllowCIDR is the list of egress destinations the tool
	// may reach. An empty list means no outbound network. "0.0.0.0/0"
	// allows everything (but still requires a network namespace with
	// a working route, which Apply doesn't set up yet).
	NetworkAllowCIDR []string

	// DangerousCmdsDeny is a hard deny-list applied before argv
	// substitution. Exact string match against the joined argv. Used
	// for operator-override tools whose allowed_paths include "*".
	DangerousCmdsDeny []string

	// EnvWhitelist names env vars visible to the subprocess. Empty
	// → subprocess sees no env. The executor already enforces this
	// via buildEnv; Policy just carries the config through.
	EnvWhitelist []string

	// CPUQuota in millicpus (2000 = 2 cores). 0 = unlimited.
	// Enforced via cgroup v2 cpu.max — install deferred.
	CPUQuota int

	// MemoryLimitMB in mebibytes. 0 = unlimited. Enforced via
	// cgroup v2 memory.max — install deferred.
	MemoryLimitMB int

	// Namespaces selects which Linux namespaces to create for the
	// subprocess. Each field maps to a CLONE_NEW* flag. User namespace
	// enables unprivileged operation of the others on most kernels.
	Namespaces NamespaceSet

	// NoNewPrivs sets PR_SET_NO_NEW_PRIVS on the subprocess. Blocks
	// set-uid binaries and capability elevation. Strongly recommended.
	// Default: true (see Normalise).
	NoNewPrivs bool

	// Seccomp policy. Zero value = DefaultDenyList applied. Install
	// itself is deferred.
	Seccomp SeccompPolicy
}

// NamespaceSet selects CLONE_NEW* flags. User namespace is the gate —
// on most modern kernels (Debian 11+, Ubuntu 24+, Fedora 32+, Arch)
// unprivileged processes can create user namespaces, which enables
// the others without root.
type NamespaceSet struct {
	User    bool
	Mount   bool
	PID     bool
	Network bool
	UTS     bool
	IPC     bool
}

// Enabled reports whether any namespace flag is set — a cheap
// "should Apply do anything namespace-related" check.
func (n NamespaceSet) Enabled() bool {
	return n.User || n.Mount || n.PID || n.Network || n.UTS || n.IPC
}

// Normalise fills in sensible defaults for a Policy the caller has
// opted into. Only triggers if some sandboxing is actually enabled —
// a zero-value Policy stays zero-value (the "no sandbox" case).
func (p *Policy) Normalise() {
	sandboxEnabled := p.Namespaces.Enabled() || p.Seccomp.HasRules() || p.CPUQuota > 0 || p.MemoryLimitMB > 0
	if !sandboxEnabled {
		return
	}
	if !p.NoNewPrivs {
		p.NoNewPrivs = true
	}
	if p.Seccomp.IsZero() {
		p.Seccomp = DefaultSeccompPolicy
	}
}

// Validate returns an error if the policy is internally inconsistent.
// Callers call this at config-load time so bad config fails fast.
func (p *Policy) Validate() error {
	for _, path := range p.AllowedPaths {
		if len(path) == 0 || path[0] != '/' {
			return fmt.Errorf("AllowedPaths: %q is not absolute", path)
		}
	}
	allowed := make(map[string]struct{}, len(p.AllowedPaths))
	for _, path := range p.AllowedPaths {
		allowed[path] = struct{}{}
	}
	for _, path := range p.ReadOnlyPaths {
		if _, ok := allowed[path]; !ok {
			return fmt.Errorf("ReadOnlyPaths: %q is not in AllowedPaths", path)
		}
	}
	if p.CPUQuota < 0 {
		return errors.New("CPUQuota must be >= 0")
	}
	if p.MemoryLimitMB < 0 {
		return errors.New("MemoryLimitMB must be >= 0")
	}
	return nil
}
