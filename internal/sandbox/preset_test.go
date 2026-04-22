package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParsePathRuleDefaultsToReadOnly(t *testing.T) {
	t.Parallel()
	r, err := ParsePathRule("/usr/bin")
	if err != nil {
		t.Fatal(err)
	}
	if r.Path != "/usr/bin" {
		t.Errorf("path: got %q, want /usr/bin", r.Path)
	}
	if r.Access != AccessR {
		t.Errorf("default should be read-only (AccessR), got %s", r.Access)
	}
}

func TestParsePathRuleAccessModes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		path   string
		access Access
	}{
		{"/home/user:r", "/home/user", AccessR},
		{"/home/user:rw", "/home/user", AccessRW},
		{"/home/user:rx", "/home/user", AccessRX},
		{"/home/user:rwx", "/home/user", AccessRWX},
		// Flag order shouldn't matter.
		{"/home/user:xrw", "/home/user", AccessRWX},
		{"/home/user:wr", "/home/user", AccessRW},
		// Trailing slashes preserved; no magic.
		{"~/.ssh/:r", "~/.ssh/", AccessR},
	}
	for _, tc := range cases {
		got, err := ParsePathRule(tc.in)
		if err != nil {
			t.Errorf("%q: unexpected error: %v", tc.in, err)
			continue
		}
		if got.Path != tc.path {
			t.Errorf("%q: path got %q, want %q", tc.in, got.Path, tc.path)
		}
		if got.Access != tc.access {
			t.Errorf("%q: access got %s, want %s", tc.in, got.Access, tc.access)
		}
	}
}

func TestParsePathRuleRejectsWriteOnlyOrExecOnly(t *testing.T) {
	t.Parallel()
	// 'w' or 'x' without 'r' looks like user confusion — reject so
	// we surface the typo rather than install a nonsense rule.
	// (Treated as "no flag match" → whole string becomes path.)
	r, err := ParsePathRule("/a:w")
	if err != nil {
		t.Fatal(err)
	}
	// The whole thing falls back to being treated as a path, since ":w"
	// wasn't a valid flag set. That's a clean failure mode: the loader
	// will fail downstream when it can't canonicalise "/a:w" as a path.
	if r.Path != "/a:w" {
		t.Errorf("'w'-only should fall back to treating whole string as path; got %q", r.Path)
	}
}

func TestParsePathRuleRejectsDuplicateFlags(t *testing.T) {
	t.Parallel()
	r, err := ParsePathRule("/a:rr")
	if err != nil {
		t.Fatal(err)
	}
	// Duplicate flags → not a valid flag set → treat as path.
	if r.Path != "/a:rr" {
		t.Errorf("duplicate flags should fall back; got %q", r.Path)
	}
}

func TestParsePathRuleEmptyErrors(t *testing.T) {
	t.Parallel()
	if _, err := ParsePathRule(""); err == nil {
		t.Error("empty input should error")
	}
}

func TestAccessHasAndString(t *testing.T) {
	t.Parallel()
	if !AccessRW.Has(AccessR) {
		t.Error("rw should contain r")
	}
	if AccessR.Has(AccessW) {
		t.Error("r should not contain w")
	}
	if AccessRWX.String() != "rwx" {
		t.Errorf("RWX string: got %q, want rwx", AccessRWX.String())
	}
	if AccessRW.String() != "rw" {
		t.Errorf("RW string: got %q, want rw", AccessRW.String())
	}
}

// TestBuiltinPresetsRegistered confirms the init() side-effect: every
// entry in BuiltinPresets is in the registry before any test runs.
// Regression guard for accidentally dropping init() wiring.
func TestBuiltinPresetsRegistered(t *testing.T) {
	t.Parallel()
	for _, p := range BuiltinPresets {
		got, ok := LookupPreset(p.Name)
		if !ok {
			t.Errorf("builtin preset %q not registered", p.Name)
			continue
		}
		if got.Name != p.Name {
			t.Errorf("preset %q: name mismatch", p.Name)
		}
	}
}

// TestListPresetsContainsKnownBuiltins — readable sanity check: the
// presets we ship are actually visible via the ListPresets API that
// docs / debug tooling will use.
func TestListPresetsContainsKnownBuiltins(t *testing.T) {
	t.Parallel()
	list := ListPresets()
	for _, name := range []string{"system-libs", "ssh-keys", "git-config", "tmp"} {
		if !slices.Contains(list, name) {
			t.Errorf("ListPresets missing %q; got %v", name, list)
		}
	}
}

func TestRegisterPresetOverridesBuiltin(t *testing.T) {
	// NOT parallel — we're mutating the package-level preset registry,
	// which would race with other tests that list/lookup presets.
	const name = "test-override-preset"
	original := Preset{Name: name, Description: "original", Rules: []PathRule{{"/a", AccessR}}}
	replacement := Preset{Name: name, Description: "replacement", Rules: []PathRule{{"/b", AccessRW}}}

	RegisterPreset(original)
	RegisterPreset(replacement)

	got, ok := LookupPreset(name)
	if !ok {
		t.Fatal("preset should be registered")
	}
	if got.Description != "replacement" {
		t.Errorf("second RegisterPreset should win; got %q", got.Description)
	}

	t.Cleanup(func() {
		presetRegistry.mu.Lock()
		delete(presetRegistry.presets, name)
		presetRegistry.mu.Unlock()
	})
}

// TestResolveExpandsHomeDir proves ~ expansion happens at compose
// time — critical because config files use ~ but landlock wants an
// absolute path. Skips if the test environment has no usable HOME.
func TestResolveExpandsHomeDir(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no HOME available: %v", err)
	}
	// Create a real file under HOME so EvalSymlinks succeeds.
	marker := filepath.Join(home, ".lobslaw_sandbox_test_marker")
	if err := os.WriteFile(marker, []byte{}, 0o644); err != nil {
		t.Skipf("can't create marker in HOME: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(marker) })

	rules, err := Resolve(nil, []PathRule{
		{Path: "~/.lobslaw_sandbox_test_marker", Access: AccessR},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 resolved rule, got %d", len(rules))
	}
	if !strings.HasPrefix(rules[0].Path, home) {
		t.Errorf("~ should expand to %q, got %q", home, rules[0].Path)
	}
}

// TestResolveSkipsMissingPaths — landlock's IgnoreIfMissing posture
// means a missing path is a no-op. Our Resolve mirrors that: a rule
// referencing a path that doesn't exist silently drops out.
func TestResolveSkipsMissingPaths(t *testing.T) {
	t.Parallel()
	rules, err := Resolve(nil, []PathRule{
		{Path: "/this/definitely/does/not/exist/xyz", Access: AccessRW},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 0 {
		t.Errorf("missing path should drop silently, got %v", rules)
	}
}

// TestResolveMergesDuplicatesTakingMostPermissive proves the "genuine
// duplicates → most permissive" rule. Two rules on the exact same
// realpath (/tmp) with different access should merge to the OR'd set.
func TestResolveMergesDuplicatesTakingMostPermissive(t *testing.T) {
	t.Parallel()
	rules, err := Resolve(nil, []PathRule{
		{Path: "/tmp", Access: AccessR},
		{Path: "/tmp", Access: AccessW},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 merged rule, got %v", rules)
	}
	if !rules[0].Access.Has(AccessRW) {
		t.Errorf("duplicate should union to RW, got %s", rules[0].Access)
	}
}

// TestResolveSortsLongestPathFirst — the "longest realpath wins"
// expectation is realised by landlock itself at the kernel level; we
// sort so callers iterate most-specific first for deterministic
// output (and so debug logs read sensibly).
func TestResolveSortsLongestPathFirst(t *testing.T) {
	t.Parallel()
	rules, err := Resolve(nil, []PathRule{
		{Path: "/usr", Access: AccessR},
		{Path: "/usr/local/bin", Access: AccessR},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("expected 2, got %d", len(rules))
	}
	if rules[0].Path != "/usr/local/bin" {
		t.Errorf("sort: expected /usr/local/bin first, got %v", rules)
	}
}

func TestResolveUnknownPresetErrors(t *testing.T) {
	t.Parallel()
	_, err := Resolve([]string{"no-such-preset"}, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown preset") {
		t.Errorf("expected unknown-preset error, got %v", err)
	}
}

// TestResolveSystemLibsBuiltinProducesRealPaths — end-to-end: the
// shipped system-libs preset resolves against a real Linux filesystem
// (/usr always exists; /bin and /lib are typically symlinks). At least
// some paths must survive canonicalisation or downstream landlock
// install would no-op and the default-deny posture would break
// everything.
func TestResolveSystemLibsBuiltinProducesRealPaths(t *testing.T) {
	t.Parallel()
	rules, err := Resolve([]string{"system-libs"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) == 0 {
		t.Fatal("system-libs preset should produce at least some resolved paths")
	}
	var hasUsr bool
	for _, r := range rules {
		if r.Path == "/usr" || strings.HasPrefix(r.Path, "/usr/") {
			hasUsr = true
			break
		}
	}
	if !hasUsr {
		t.Errorf("expected /usr in resolved rules, got %+v", rules)
	}
}

// TestWithPresetsProducesAllowedAndReadOnly confirms the Policy
// convenience method maps resolved rules back into Policy's native
// AllowedPaths/ReadOnlyPaths split that sandbox.Apply consumes.
func TestWithPresetsProducesAllowedAndReadOnly(t *testing.T) {
	t.Parallel()
	p := Policy{
		AllowedPaths:  []string{"/tmp"},
		ReadOnlyPaths: []string{},
	}
	out, err := p.WithPresets("system-libs")
	if err != nil {
		t.Fatal(err)
	}
	// system-libs is all RO; /tmp inline is RW.
	if !slices.Contains(out.AllowedPaths, "/tmp") {
		t.Error("inline /tmp should survive to AllowedPaths")
	}
	if slices.Contains(out.ReadOnlyPaths, "/tmp") {
		t.Error("/tmp should NOT be in ReadOnlyPaths (it's RW inline)")
	}
	// At least one system-libs path should surface as RO.
	if len(out.ReadOnlyPaths) == 0 {
		t.Errorf("system-libs should produce RO entries; got %+v", out.ReadOnlyPaths)
	}
}

func TestWithPresetsUnknownPresetReturnsOriginal(t *testing.T) {
	t.Parallel()
	p := Policy{NoNewPrivs: true}
	got, err := p.WithPresets("no-such-preset")
	if err == nil {
		t.Fatal("expected error for unknown preset")
	}
	// Original Policy should be returned unmodified (preserving NoNewPrivs).
	if !got.NoNewPrivs {
		t.Error("error path should return original Policy (with NoNewPrivs true)")
	}
}
