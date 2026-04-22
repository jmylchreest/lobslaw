package sandbox

import (
	"fmt"
)

// PolicySpec is the on-disk representation of a tool policy — the
// TOML schema for `policy.d/<tool>.toml` files. A PolicySpec is
// resolved to a runtime *Policy via ToPolicy() which walks the
// presets list, parses path:flags entries, and composes everything
// into the AllowedPaths / ReadOnlyPaths split that sandbox.Apply
// consumes.
//
// The koanf tags mirror the on-disk field names (snake_case per
// project convention); struct field names follow Go style.
type PolicySpec struct {
	// Name is the tool this policy applies to; should match the
	// filename (git.toml → "git") but not enforced here — the loader
	// verifies consistency.
	Name string `koanf:"name"`

	// Description is free-form; surfaced in debug/list output.
	Description string `koanf:"description"`

	// Presets names to compose into this policy. Unknown names are
	// a hard error at ToPolicy() time.
	Presets []string `koanf:"presets"`

	// Paths are inline "path[:flags]" entries added on top of any
	// presets. Flag suffixes: r | rw | rx | rwx. Default when missing:
	// r (read-only — principle of least privilege).
	Paths []string `koanf:"paths"`

	// NoNewPrivs sets PR_SET_NO_NEW_PRIVS. Required by Landlock; the
	// helper sets it automatically when AllowedPaths is non-empty but
	// tools without Landlock still benefit from this on its own.
	NoNewPrivs bool `koanf:"no_new_privs"`

	// NetworkAllowCIDR is the list of egress CIDRs. Passes through to
	// Policy.NetworkAllowCIDR; enforcement is deferred (see DEFERRED.md
	// for the nftables entry).
	NetworkAllowCIDR []string `koanf:"network_allow_cidr"`

	// SeccompDeny is the explicit deny-list of syscall names. If empty
	// AND SeccompDefault is true, DefaultSeccompPolicy is applied.
	SeccompDeny []string `koanf:"seccomp_deny"`

	// SeccompDefault requests DefaultSeccompPolicy (the baseline deny
	// set — ptrace, mount, bpf, keyctl, …). Mutually exclusive with
	// SeccompDeny being non-empty.
	SeccompDefault bool `koanf:"seccomp_default"`

	// Namespaces selects CLONE_NEW* flags. See docs/SANDBOX.md for
	// which fit which use cases.
	Namespaces NamespaceSpec `koanf:"namespaces"`
}

// NamespaceSpec is the koanf/TOML-friendly form of NamespaceSet.
type NamespaceSpec struct {
	User    bool `koanf:"user"`
	Mount   bool `koanf:"mount"`
	PID     bool `koanf:"pid"`
	Network bool `koanf:"network"`
	UTS     bool `koanf:"uts"`
	IPC     bool `koanf:"ipc"`
}

// PresetSpec is the on-disk shape of `policy.d/_presets/<name>.toml`.
// Deliberately narrower than PolicySpec: a preset is just a named
// bundle of path rules, nothing more. Separate type prevents users
// from setting tool-only fields like namespaces in a preset file
// and wondering why they don't take effect.
type PresetSpec struct {
	Name        string   `koanf:"name"`
	Description string   `koanf:"description"`
	Paths       []string `koanf:"paths"`
}

// ToPolicy resolves the spec to a concrete runtime Policy. Walks
// the presets list via Resolve, parses inline Paths, and fills in
// the other fields (namespaces, seccomp, etc.).
func (s *PolicySpec) ToPolicy() (*Policy, error) {
	inline, err := parsePathList(s.Paths)
	if err != nil {
		return nil, fmt.Errorf("policy %q: %w", s.Name, err)
	}
	resolved, err := Resolve(s.Presets, inline)
	if err != nil {
		return nil, fmt.Errorf("policy %q: %w", s.Name, err)
	}

	p := &Policy{
		NoNewPrivs:       s.NoNewPrivs,
		NetworkAllowCIDR: s.NetworkAllowCIDR,
		Namespaces: NamespaceSet{
			User:    s.Namespaces.User,
			Mount:   s.Namespaces.Mount,
			PID:     s.Namespaces.PID,
			Network: s.Namespaces.Network,
			UTS:     s.Namespaces.UTS,
			IPC:     s.Namespaces.IPC,
		},
	}

	for _, r := range resolved {
		p.AllowedPaths = append(p.AllowedPaths, r.Path)
		if !r.Access.Has(AccessW) {
			p.ReadOnlyPaths = append(p.ReadOnlyPaths, r.Path)
		}
	}

	switch {
	case len(s.SeccompDeny) > 0 && s.SeccompDefault:
		return nil, fmt.Errorf("policy %q: seccomp_deny and seccomp_default are mutually exclusive", s.Name)
	case len(s.SeccompDeny) > 0:
		p.Seccomp = SeccompPolicy{Deny: s.SeccompDeny}
	case s.SeccompDefault:
		p.Seccomp = DefaultSeccompPolicy
	}

	return p, nil
}

// ToPreset turns a PresetSpec into the internal Preset type. Paths
// are parsed but NOT canonicalised — canonicalisation happens at
// Resolve time (after the preset composes with others) so ~ and
// symlinks are resolved once, at the real compose site.
func (s *PresetSpec) ToPreset() (Preset, error) {
	rules, err := parsePathList(s.Paths)
	if err != nil {
		return Preset{}, fmt.Errorf("preset %q: %w", s.Name, err)
	}
	return Preset{
		Name:        s.Name,
		Description: s.Description,
		Rules:       rules,
	}, nil
}

// parsePathList is the shared path:flags parser for both policy and
// preset specs. Preserves order so "most-specific wins" only has to
// be computed once at Resolve time.
func parsePathList(entries []string) ([]PathRule, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make([]PathRule, 0, len(entries))
	for _, e := range entries {
		r, err := ParsePathRule(e)
		if err != nil {
			return nil, fmt.Errorf("parse %q: %w", e, err)
		}
		out = append(out, r)
	}
	return out, nil
}
