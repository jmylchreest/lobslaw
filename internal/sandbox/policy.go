package sandbox

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
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
	// AllowedPaths are RW paths visible to the sandbox. Landlock
	// enforces the restriction once Phase 4.5.5 lands. Paths must be
	// absolute; verified by Validate.
	//
	// Legacy field, retained for back-compat. New callers should
	// populate Mounts instead — it expresses per-path read/write/exec
	// independently, which matches the storage MountMode (rwx) model
	// the rest of the codebase uses.
	AllowedPaths []string `json:"allowed_paths,omitempty"`

	// ReadOnlyPaths subset of AllowedPaths restricted to read access.
	// Every entry MUST appear in AllowedPaths. Verified by Validate.
	ReadOnlyPaths []string `json:"read_only_paths,omitempty"`

	// Mounts is the per-path rwx mode list that drives Landlock when
	// populated. Preferred over AllowedPaths/ReadOnlyPaths because it
	// distinguishes "rx" (read+exec, no write) from "rw" (read+write,
	// no exec) cleanly, matching the storage layer's MountMode. The
	// install layer prefers Mounts over the legacy fields when both
	// are set; mixing them is allowed (additive) but not recommended.
	Mounts []PolicyMount `json:"mounts,omitempty"`

	// NetworkAllowCIDR is the list of egress destinations the tool
	// may reach. Empty → no outbound network beyond loopback (when
	// NetworkFilter enforces). The list flows through to
	// internal/sandbox/netfilter.RuleSet.AllowCIDRs.
	NetworkAllowCIDR []string `json:"network_allow_cidr,omitempty"`

	// NetworkFilter requests kernel-level egress enforcement via
	// nftables in the subprocess's network namespace. Linux-only;
	// requires Namespaces.Network = true. When set, the apply
	// pipeline installs a drop-by-default output chain plus
	// allow-rules for loopback (smokescreen UDS), DNS (when
	// NetworkAllowDNS is true), and every NetworkAllowCIDR entry.
	NetworkFilter bool `json:"network_filter,omitempty"`

	// NetworkAllowDNS opens UDP+TCP port 53 to any destination when
	// NetworkFilter is on. Most skills need DNS to resolve hostnames
	// before the application-layer HTTPS_PROXY connects. Off by
	// default — operators turn it on per-skill via policy.
	NetworkAllowDNS bool `json:"network_allow_dns,omitempty"`

	// DangerousCmdsDeny hard deny-list applied before argv
	// substitution. Exact string match against the joined argv.
	DangerousCmdsDeny []string `json:"dangerous_cmds_deny,omitempty"`

	// EnvWhitelist names env vars visible to the subprocess. Empty →
	// empty env. Enforced by the executor's buildEnv, not the sandbox.
	EnvWhitelist []string `json:"env_whitelist,omitempty"`

	// CPUQuota in millicpus (2000 = 2 cores). 0 = unlimited. Enforced
	// via cgroup v2 cpu.max — install deferred.
	CPUQuota int `json:"cpu_quota,omitempty"`

	// MemoryLimitMB in mebibytes. 0 = unlimited. Enforced via
	// cgroup v2 memory.max — install deferred.
	MemoryLimitMB int `json:"memory_limit_mb,omitempty"`

	// Namespaces selects which Linux namespaces to create for the
	// subprocess. Maps to CLONE_NEW* flags. User namespace enables
	// unprivileged operation of the others on most modern kernels.
	Namespaces NamespaceSet `json:"namespaces,omitzero"`

	// NoNewPrivs sets PR_SET_NO_NEW_PRIVS on the subprocess. Blocks
	// setuid binaries and capability elevation. Required by Landlock
	// (the install sets it automatically). Default true when any
	// sandboxing is enabled; see Normalise.
	NoNewPrivs bool `json:"no_new_privs,omitempty"`

	// Seccomp policy. Zero value → DefaultSeccompPolicy applied by
	// Normalise when sandboxing is enabled.
	Seccomp SeccompPolicy `json:"seccomp,omitzero"`
}

// PolicyMount is one filesystem area the subprocess may access,
// expressed as a path + a read/write/exec mask. Mirrors the storage
// layer's MountMode so Landlock enforcement and mount-resolver
// enforcement use the same vocabulary.
type PolicyMount struct {
	Path  string `json:"path"`
	Read  bool   `json:"read,omitempty"`
	Write bool   `json:"write,omitempty"`
	Exec  bool   `json:"exec,omitempty"`
}

// NamespaceSet selects CLONE_NEW* flags. User namespace is the gate —
// on most modern kernels (Debian 11+, Ubuntu 24+, Fedora 32+, Arch)
// unprivileged processes can create user namespaces, which enables
// the others without root.
type NamespaceSet struct {
	User    bool `json:"user,omitempty"`
	Mount   bool `json:"mount,omitempty"`
	PID     bool `json:"pid,omitempty"`
	Network bool `json:"network,omitempty"`
	UTS     bool `json:"uts,omitempty"`
	IPC     bool `json:"ipc,omitempty"`
}

// Enabled reports whether any namespace flag is set — a cheap
// "should Apply do anything namespace-related" check.
func (n NamespaceSet) Enabled() bool {
	return n.User || n.Mount || n.PID || n.Network || n.UTS || n.IPC
}

// Normalise fills in sensible defaults for enforcement layers the
// caller has opted into. Only fires when the caller has asked for
// active enforcement — NoNewPrivs, Landlock (AllowedPaths), or
// explicit Seccomp rules. Namespaces and resource quotas alone are
// orthogonal isolation mechanisms and don't auto-enable seccomp /
// NoNewPrivs, so callers can test the namespace path without
// dragging in the full reexec helper pipeline.
//
// A zero-value Policy stays zero-value (the "no sandbox" case).
func (p *Policy) Normalise() {
	enforcementRequested := p.NoNewPrivs || len(p.AllowedPaths) > 0 || len(p.Mounts) > 0 || p.Seccomp.HasRules()
	if !enforcementRequested {
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
	for i, m := range p.Mounts {
		// ST1005 reads the leading "Mounts" as a capitalised sentence,
		// but it is the Policy struct field name — the same as the
		// "ReadOnlyPaths:" prefix above, which the check accepts only
		// because its inner caps make it look like an identifier.
		// Naming the offending field is the point of these messages.
		if len(m.Path) == 0 || m.Path[0] != '/' {
			return fmt.Errorf("Mounts[%d]: %q is not absolute", i, m.Path) //nolint:staticcheck // ST1005: "Mounts" is a struct field name
		}
		if !m.Read && !m.Write && !m.Exec {
			return fmt.Errorf("Mounts[%d]: %q has no access bits set", i, m.Path) //nolint:staticcheck // ST1005: "Mounts" is a struct field name
		}
	}
	if p.CPUQuota < 0 {
		return errors.New("CPUQuota must be >= 0")
	}
	if p.MemoryLimitMB < 0 {
		return errors.New("MemoryLimitMB must be >= 0")
	}
	if p.NetworkFilter && !p.Namespaces.Network {
		return errors.New("NetworkFilter requires Namespaces.Network")
	}
	return nil
}

// --- in-process evaluation --------------------------------------------

// AllowsPath reports whether this policy permits `need` on path.
//
// `need` is the same Access bitmask the path-rule parser produces from
// a "path:rw" line, so the thing an operator writes and the thing a
// builtin asks for are one vocabulary rather than two.
//
// It exists because Landlock cannot help an in-process builtin.
// Landlock confines a process, and the process running read_file IS
// lobslaw — confining it would confine the agent, its Raft client and
// its provider connections along with the tool. So the subprocess
// tools get kernel enforcement from exactly this struct, and the
// in-process ones have to evaluate the same struct in Go.
//
// Same struct on purpose. Two implementations of "what did the policy
// say" is one implementation and one bug, and the bug is invisible
// until someone compares a confined `git` against an unconfined
// `read_file` and finds they disagree about a path the operator wrote
// down once.
//
// # What a nil or empty policy means
//
// True. A tool with no policy is unconfined BY THIS LAYER, which is
// the existing behaviour for subprocesses (sandbox.Apply is a no-op on
// a nil Policy) and the only reading that keeps this additive.
//
// That is safe only because of the contract below, and would be a hole
// without it.
//
// # This layer may only SUBTRACT
//
// The mount resolver, the internal-path list and the hardline floor
// run FIRST and are not reachable from a policy file. A policy can
// narrow what those already allowed; it can never widen it.
//
// The direction matters more than it looks. policy.d is a directory of
// files, hot-reloaded, under paths that include one inside the
// operator's home — and the agent runs as that user. If a policy file
// could grant reach, then an agent that talks a user into running a
// shell has a supported, documented, auto-reloading route to reading
// the memory key. Subtract-only means the worst a hostile policy file
// can do is break a tool, which is noisy and recoverable.
func (p *Policy) AllowsPath(path string, need Access) bool {
	if p == nil {
		return true
	}
	// A policy that names no filesystem area is not a filesystem
	// policy — it may be seccomp or namespaces only. Reading it as
	// "deny everything" would silently disable every path-taking tool
	// the moment an operator confined one of them by syscall.
	if len(p.Mounts) == 0 && len(p.AllowedPaths) == 0 {
		return true
	}
	if need == 0 {
		return true
	}

	clean := filepath.Clean(path)
	for _, m := range p.effectiveMounts() {
		if !pathWithin(clean, m.Path) {
			continue
		}
		// First containing entry decides, and it must satisfy EVERY
		// requested bit. A wider entry later in the list does not
		// rescue a narrower one that already matched — otherwise
		// ordering, not intent, decides whether a write is allowed.
		return mountAccess(m).Has(need)
	}
	return false
}

// effectiveMounts renders the legacy AllowedPaths/ReadOnlyPaths pair
// in the Mounts vocabulary, so AllowsPath has one shape to reason
// about. Mounts first, matching the install layer's stated preference
// when both are set.
//
// Longest path first, so /workspace/secrets:r is consulted before
// /workspace:rw rather than being shadowed by it. Without the sort the
// answer depends on the order somebody happened to write the file in,
// which for a confinement policy is not a defensible way to decide.
func (p *Policy) effectiveMounts() []PolicyMount {
	out := make([]PolicyMount, 0, len(p.Mounts)+len(p.AllowedPaths))
	out = append(out, p.Mounts...)

	readOnly := make(map[string]struct{}, len(p.ReadOnlyPaths))
	for _, path := range p.ReadOnlyPaths {
		readOnly[path] = struct{}{}
	}
	for _, path := range p.AllowedPaths {
		if _, ro := readOnly[path]; ro {
			out = append(out, PolicyMount{Path: path, Read: true})
			continue
		}
		out = append(out, PolicyMount{Path: path, Read: true, Write: true})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return len(out[i].Path) > len(out[j].Path)
	})
	return out
}

// pathWithin reports whether path is root or sits underneath it.
//
// Separator-aware, so "/workspaces" is not inside "/workspace" — a
// prefix test alone is the classic way a confinement boundary leaks to
// the directory next door.
func pathWithin(path, root string) bool {
	root = filepath.Clean(root)
	if path == root {
		return true
	}
	if root == string(filepath.Separator) {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// mountAccess renders a PolicyMount's three bools as the Access mask
// the rest of the package speaks.
func mountAccess(m PolicyMount) Access {
	var a Access
	if m.Read {
		a |= AccessR
	}
	if m.Write {
		a |= AccessW
	}
	if m.Exec {
		a |= AccessX
	}
	return a
}
