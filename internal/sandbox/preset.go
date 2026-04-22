package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Access encodes the filesystem access mode requested for a path.
// Maps onto Landlock rule kinds:
//
//	AccessR   → ROFiles / RODirs  (read + execute for dirs)
//	AccessRW  → RWFiles / RWDirs  (read + write + execute)
//	AccessRX  → RODirs            (treated as read; landlock V5 RODirs
//	                               already grants execute, so r+x is r)
//	AccessRWX → RWDirs            (full access — RWDirs grants execute)
//
// Defaulting to AccessR (read-only) reflects the principle of least
// privilege: a path entry without an explicit `:flags` suffix is
// read-only.
type Access uint8

const (
	AccessR   Access = 1 << iota // read
	AccessW                      // write
	AccessX                      // execute
	AccessRO  = AccessR
	AccessRW  = AccessR | AccessW
	AccessRX  = AccessR | AccessX
	AccessRWX = AccessR | AccessW | AccessX
)

// Has reports whether the access set contains every bit of want.
func (a Access) Has(want Access) bool { return a&want == want }

// String returns the canonical "rwx"-style spelling for diagnostics
// and round-trip serialisation.
func (a Access) String() string {
	var b strings.Builder
	if a.Has(AccessR) {
		b.WriteByte('r')
	}
	if a.Has(AccessW) {
		b.WriteByte('w')
	}
	if a.Has(AccessX) {
		b.WriteByte('x')
	}
	if b.Len() == 0 {
		return "(none)"
	}
	return b.String()
}

// PathRule pairs an absolute path with the access requested. Built
// by ParsePathRule from "path[:flags]" strings.
type PathRule struct {
	Path   string
	Access Access
}

// ParsePathRule parses a "path[:flags]" entry. Flags must be one of
// {r, rw, rx, rwx}; missing flags default to "r" (read-only — least
// privilege). The path component is left as written (~ expansion and
// canonicalisation happen at compose-time via Resolve).
//
// We split on the LAST ':' so paths can theoretically contain ':'
// characters elsewhere; on Linux this is rare but legal.
func ParsePathRule(s string) (PathRule, error) {
	if s == "" {
		return PathRule{}, fmt.Errorf("empty path rule")
	}
	idx := strings.LastIndex(s, ":")
	// No colon, or the colon is part of an absolute path with no flag
	// suffix (e.g. trailing slash on a path like "/etc/" — no colon).
	// If the segment after ':' isn't a known flag set, treat the whole
	// string as a path with default-read access.
	if idx < 0 {
		return PathRule{Path: s, Access: AccessR}, nil
	}
	pathPart, flagPart := s[:idx], s[idx+1:]
	access, ok := parseAccessFlags(flagPart)
	if !ok {
		// Maybe ':' was part of the path; treat the whole string as path.
		return PathRule{Path: s, Access: AccessR}, nil
	}
	if pathPart == "" {
		return PathRule{}, fmt.Errorf("path rule %q: empty path before flags", s)
	}
	return PathRule{Path: pathPart, Access: access}, nil
}

// parseAccessFlags maps a flag string (lowercase r/w/x in any order
// without duplicates) to an Access set. Returns ok=false for input
// outside {r, rw, rx, rwx} so ParsePathRule can fall back to "the
// colon was part of the path".
func parseAccessFlags(s string) (Access, bool) {
	if s == "" || len(s) > 3 {
		return 0, false
	}
	var got Access
	seen := [256]bool{}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if seen[c] {
			return 0, false
		}
		seen[c] = true
		switch c {
		case 'r':
			got |= AccessR
		case 'w':
			got |= AccessW
		case 'x':
			got |= AccessX
		default:
			return 0, false
		}
	}
	// Reject 'w' or 'x' alone — user almost certainly meant rw/rx.
	if got != 0 && !got.Has(AccessR) {
		return 0, false
	}
	return got, true
}

// Preset is a named bundle of PathRules that can be referenced by
// other policies. Built-ins live in BuiltinPresets; operators may
// register additional presets or override built-ins via the
// .policy.toml loader (Phase 4.5.6c).
type Preset struct {
	Name        string
	Description string
	Rules       []PathRule
}

// presetRegistry holds the merged catalogue of built-in + operator
// presets. Mutated only at startup via RegisterPreset.
var presetRegistry = struct {
	mu      sync.RWMutex
	presets map[string]Preset
}{presets: make(map[string]Preset)}

// RegisterPreset adds (or overrides) a named preset. Operator .toml
// files use this; the boot path also calls it for built-ins. Last
// write wins so operator-supplied files can shadow defaults.
func RegisterPreset(p Preset) {
	presetRegistry.mu.Lock()
	defer presetRegistry.mu.Unlock()
	presetRegistry.presets[p.Name] = p
}

// LookupPreset returns the preset with the given name, or
// (zero, false) if missing.
func LookupPreset(name string) (Preset, bool) {
	presetRegistry.mu.RLock()
	defer presetRegistry.mu.RUnlock()
	p, ok := presetRegistry.presets[name]
	return p, ok
}

// ListPresets returns all registered preset names sorted alphabetically.
// Used by docs / debug output / `lobslaw sandbox presets list`.
func ListPresets() []string {
	presetRegistry.mu.RLock()
	defer presetRegistry.mu.RUnlock()
	names := make([]string, 0, len(presetRegistry.presets))
	for name := range presetRegistry.presets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// init seeds BuiltinPresets into the registry. Operator .policy.toml
// files (loaded after boot) may overwrite any of these by registering
// a preset with the same name.
func init() {
	for _, p := range BuiltinPresets {
		RegisterPreset(p)
	}
}

// BuiltinPresets is the catalogue shipped in-binary. The set is
// deliberately conservative — every preset is read-only by default;
// operators compose with explicit `path:rw` overrides for paths a
// tool needs to write to. See docs/SANDBOX.md for recipes.
var BuiltinPresets = []Preset{
	{
		Name:        "system-libs",
		Description: "OS executables + shared libraries (RO)",
		Rules: []PathRule{
			{"/usr", AccessR},
			{"/bin", AccessR},
			{"/sbin", AccessR},
			{"/lib", AccessR},
			{"/lib64", AccessR},
		},
	},
	{
		Name:        "system-certs",
		Description: "TLS CA bundles for HTTPS (RO)",
		Rules: []PathRule{
			{"/etc/ssl", AccessR},
			{"/etc/ca-certificates", AccessR},
			{"/etc/pki", AccessR},
		},
	},
	{
		Name:        "dns",
		Description: "DNS resolver config + hosts file (RO)",
		Rules: []PathRule{
			{"/etc/resolv.conf", AccessR},
			{"/etc/nsswitch.conf", AccessR},
			{"/etc/hosts", AccessR},
		},
	},
	{
		Name:        "tmp",
		Description: "/tmp scratch space (RW)",
		Rules: []PathRule{
			{"/tmp", AccessRW},
		},
	},
	{
		Name:        "home-config",
		Description: "User config dir under ~/.config (RO)",
		Rules: []PathRule{
			{"~/.config", AccessR},
		},
	},
	{
		Name:        "git-config",
		Description: "Git config + global hooks (RO)",
		Rules: []PathRule{
			{"~/.gitconfig", AccessR},
			{"~/.config/git", AccessR},
		},
	},
	{
		Name:        "ssh-keys",
		Description: "SSH private + public keys + known_hosts (RO)",
		Rules: []PathRule{
			{"~/.ssh", AccessR},
		},
	},
	{
		Name:        "gpg-keys",
		Description: "GPG keyring (RO)",
		Rules: []PathRule{
			{"~/.gnupg", AccessR},
		},
	},
	{
		Name:        "aws-creds",
		Description: "AWS credentials and config (RO)",
		Rules: []PathRule{
			{"~/.aws", AccessR},
		},
	},
}

// Resolve takes a list of preset names + inline rules and returns
// the composed list of canonicalised PathRules suitable for the
// landlock install.
//
// Composition rules per design (`docs/SANDBOX.md`):
//
//   - `~` expands to the agent's home dir at compose-time via
//     os.UserHomeDir() — single-tenant model, no per-user resolution.
//   - All paths canonicalised via filepath.EvalSymlinks (silently
//     dropped if the path doesn't exist — matches landlock's
//     IgnoreIfMissing posture).
//   - Longest-realpath wins for prefix conflicts (e.g. /usr/local/app
//     wins over /usr).
//   - Exact realpath duplicates → most permissive access wins
//     (rw beats r, rwx beats rw).
//
// Returns the resolved rules sorted by path-length descending so
// callers can iterate in "most-specific first" order.
func Resolve(presetNames []string, inline []PathRule) ([]PathRule, error) {
	merged := make([]PathRule, 0, len(inline)*2)
	for _, name := range presetNames {
		p, ok := LookupPreset(name)
		if !ok {
			return nil, fmt.Errorf("unknown preset %q", name)
		}
		merged = append(merged, p.Rules...)
	}
	merged = append(merged, inline...)

	expanded, err := expandAndCanonicalise(merged)
	if err != nil {
		return nil, err
	}
	return mergeByRealpath(expanded), nil
}

// expandAndCanonicalise turns each rule's textual path into an
// absolute, symlink-resolved path. Missing paths are dropped (not
// errors) — landlock will skip them gracefully via IgnoreIfMissing,
// and dropping here keeps the resolved list minimal.
func expandAndCanonicalise(rules []PathRule) ([]PathRule, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		// Without a home dir, ~ expansion fails — but rules without ~
		// can still be resolved. Keep going; mark home as empty so
		// any ~ rule errors loudly.
		home = ""
	}
	out := make([]PathRule, 0, len(rules))
	for _, r := range rules {
		path := r.Path
		if strings.HasPrefix(path, "~/") || path == "~" {
			if home == "" {
				return nil, fmt.Errorf("rule %q uses ~ but $HOME / os.UserHomeDir() is unavailable", r.Path)
			}
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("rule %q resolves to non-absolute path %q", r.Path, path)
		}
		// EvalSymlinks fails on missing paths — that's a feature: we
		// don't want to landlock-grant access to a path that doesn't
		// exist (could be created later as a symlink to /etc/passwd).
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			// Silently skip missing paths; landlock would no-op anyway.
			continue
		}
		out = append(out, PathRule{Path: canonical, Access: r.Access})
	}
	return out, nil
}

// mergeByRealpath collapses exact-path duplicates by taking the
// most-permissive Access (rw beats r, etc.), then sorts by path
// length descending so callers iterate "most-specific first".
//
// The longest-realpath-wins property at the policy *application*
// layer is implicit: landlock's per-path rules already work on a
// "deepest matching rule wins" basis at the kernel level, so by
// passing the full set of canonicalised rules we get the right
// behaviour for free. The sort is just to make iteration order
// deterministic and human-readable.
func mergeByRealpath(rules []PathRule) []PathRule {
	dedup := make(map[string]Access, len(rules))
	for _, r := range rules {
		dedup[r.Path] = dedup[r.Path] | r.Access
	}
	out := make([]PathRule, 0, len(dedup))
	for path, access := range dedup {
		out = append(out, PathRule{Path: path, Access: access})
	}
	sort.Slice(out, func(i, j int) bool {
		// Longer paths first (most-specific first); ties broken by name.
		if len(out[i].Path) != len(out[j].Path) {
			return len(out[i].Path) > len(out[j].Path)
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// WithPresets returns a copy of the receiver with the named presets
// resolved into AllowedPaths / ReadOnlyPaths. Inline rules supplied
// via Policy.AllowedPaths (treated as RW) and Policy.ReadOnlyPaths
// (RO) are honoured — they're added as PathRules and composed with
// the preset rules per the rules in Resolve.
//
// Returns the input on resolution error so callers can surface it
// alongside the policy that was attempted.
func (p Policy) WithPresets(names ...string) (Policy, error) {
	inline := make([]PathRule, 0, len(p.AllowedPaths))
	roSet := make(map[string]struct{}, len(p.ReadOnlyPaths))
	for _, ro := range p.ReadOnlyPaths {
		roSet[ro] = struct{}{}
	}
	for _, allow := range p.AllowedPaths {
		access := AccessRW
		if _, isRO := roSet[allow]; isRO {
			access = AccessR
		}
		inline = append(inline, PathRule{Path: allow, Access: access})
	}

	resolved, err := Resolve(names, inline)
	if err != nil {
		return p, err
	}

	out := p
	out.AllowedPaths = nil
	out.ReadOnlyPaths = nil
	for _, r := range resolved {
		out.AllowedPaths = append(out.AllowedPaths, r.Path)
		if !r.Access.Has(AccessW) {
			out.ReadOnlyPaths = append(out.ReadOnlyPaths, r.Path)
		}
	}
	return out, nil
}
