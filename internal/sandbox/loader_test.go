package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// writePolicyFile is a t.Helper for test-fixtures.
func writePolicyFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPolicySpecToPolicyInlinePaths(t *testing.T) {
	t.Parallel()
	spec := &PolicySpec{
		Name:       "test",
		Paths:      []string{"/tmp:rw", "/usr:r"},
		NoNewPrivs: true,
	}
	p, err := spec.ToPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if !p.NoNewPrivs {
		t.Error("NoNewPrivs didn't transfer")
	}
	if !slices.Contains(p.AllowedPaths, "/tmp") {
		t.Errorf("/tmp should be in AllowedPaths; got %v", p.AllowedPaths)
	}
	if slices.Contains(p.ReadOnlyPaths, "/tmp") {
		t.Errorf("/tmp is RW; should NOT be in ReadOnlyPaths")
	}
	if !slices.Contains(p.ReadOnlyPaths, "/usr") {
		t.Errorf("/usr is RO; should be in ReadOnlyPaths; got %v", p.ReadOnlyPaths)
	}
}

func TestPolicySpecSeccompDefaultAppliesDefaultPolicy(t *testing.T) {
	t.Parallel()
	spec := &PolicySpec{Name: "test", SeccompDefault: true}
	p, err := spec.ToPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if !p.Seccomp.HasRules() {
		t.Error("seccomp_default=true should populate DefaultSeccompPolicy")
	}
	if !slices.Contains(p.Seccomp.Deny, "ptrace") {
		t.Error("default seccomp should deny ptrace")
	}
}

func TestPolicySpecSeccompDenyAndDefaultAreMutuallyExclusive(t *testing.T) {
	t.Parallel()
	spec := &PolicySpec{
		Name:           "test",
		SeccompDeny:    []string{"ptrace"},
		SeccompDefault: true,
	}
	if _, err := spec.ToPolicy(); err == nil {
		t.Fatal("expected mutex error on seccomp_deny + seccomp_default")
	}
}

func TestPolicySpecNamespacesSurface(t *testing.T) {
	t.Parallel()
	spec := &PolicySpec{
		Name:       "test",
		Namespaces: NamespaceSpec{User: true, Mount: true, PID: true},
	}
	p, err := spec.ToPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if !p.Namespaces.User || !p.Namespaces.Mount || !p.Namespaces.PID {
		t.Errorf("namespaces didn't transfer: %+v", p.Namespaces)
	}
}

func TestPolicySpecUnknownPresetErrors(t *testing.T) {
	t.Parallel()
	spec := &PolicySpec{Name: "test", Presets: []string{"no-such-preset"}}
	_, err := spec.ToPolicy()
	if err == nil || !strings.Contains(err.Error(), "unknown preset") {
		t.Errorf("expected unknown-preset error, got %v", err)
	}
}

func TestPresetSpecToPreset(t *testing.T) {
	t.Parallel()
	spec := &PresetSpec{
		Name:  "my-code",
		Paths: []string{"/srv/code:rw", "/home/user/code:rw"},
	}
	p, err := spec.ToPreset()
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "my-code" {
		t.Errorf("name lost")
	}
	if len(p.Rules) != 2 {
		t.Errorf("expected 2 rules, got %d", len(p.Rules))
	}
	for _, r := range p.Rules {
		if !r.Access.Has(AccessRW) {
			t.Errorf("rule %+v should be RW", r)
		}
	}
}

func TestLoadPolicyDirMissingIsNotError(t *testing.T) {
	t.Parallel()
	result, err := LoadPolicyDir("/no/such/dir/anywhere", LoadOptions{})
	if err != nil {
		t.Errorf("missing dir should return empty result, not error: %v", err)
	}
	if result == nil || len(result.Policies) != 0 {
		t.Errorf("expected empty result, got %+v", result)
	}
}

func TestLoadPolicyDirEmptyStringReturnsEmpty(t *testing.T) {
	t.Parallel()
	result, err := LoadPolicyDir("", LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Policies) != 0 {
		t.Errorf("empty dir path should return no policies; got %v", result.Policies)
	}
}

func TestLoadPolicyDirLoadsToolPolicies(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePolicyFile(t, filepath.Join(dir, "git.toml"), `
name = "git"
description = "git ops"
paths = ["/tmp:rw"]
no_new_privs = true
`)
	writePolicyFile(t, filepath.Join(dir, "rsync.toml"), `
name = "rsync"
paths = ["/var/backups:rw"]
`)

	result, err := LoadPolicyDir(dir, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Policies) != 2 {
		t.Errorf("expected 2 policies, got %d: %v", len(result.Policies), result.Policies)
	}
	if p, ok := result.Policies["git"]; !ok || !p.NoNewPrivs {
		t.Errorf("git policy missing or malformed: %+v", p)
	}
	if _, ok := result.Policies["rsync"]; !ok {
		t.Error("rsync policy missing")
	}
}

func TestLoadPolicyDirRejectsFilenameMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePolicyFile(t, filepath.Join(dir, "git.toml"), `
name = "rsync"
`)
	_, err := LoadPolicyDir(dir, LoadOptions{})
	if err == nil || !strings.Contains(err.Error(), "doesn't match filename") {
		t.Errorf("expected name-mismatch error, got %v", err)
	}
}

func TestLoadPolicyDirLoadsAndRegistersPresets(t *testing.T) {
	// NOT parallel — mutates package-level preset registry.
	dir := t.TempDir()
	const presetName = "test-loader-preset"
	writePolicyFile(t, filepath.Join(dir, "_presets", presetName+".toml"), `
name = "`+presetName+`"
description = "loader test preset"
paths = ["/srv/code:rw"]
`)

	t.Cleanup(func() {
		presetRegistry.mu.Lock()
		delete(presetRegistry.presets, presetName)
		presetRegistry.mu.Unlock()
	})

	result, err := LoadPolicyDir(dir, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(result.PresetsLoaded, presetName) {
		t.Errorf("expected preset %q in PresetsLoaded, got %v", presetName, result.PresetsLoaded)
	}
	// Confirm it's actually in the registry now.
	if _, ok := LookupPreset(presetName); !ok {
		t.Error("preset wasn't registered")
	}
}

func TestLoadPolicyDirOperatorPresetOverridesBuiltin(t *testing.T) {
	// NOT parallel — mutates package-level preset registry.
	dir := t.TempDir()
	// Override the built-in `tmp` preset with a RO version.
	writePolicyFile(t, filepath.Join(dir, "_presets", "tmp.toml"), `
name = "tmp"
description = "operator override: RO /tmp"
paths = ["/tmp:r"]
`)

	// Snapshot original so we can restore.
	original, _ := LookupPreset("tmp")
	t.Cleanup(func() { RegisterPreset(original) })

	result, err := LoadPolicyDir(dir, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(result.OverriddenBuiltins, "tmp") {
		t.Errorf("expected OverriddenBuiltins to include 'tmp'; got %v", result.OverriddenBuiltins)
	}
	got, _ := LookupPreset("tmp")
	if got.Description != "operator override: RO /tmp" {
		t.Errorf("override didn't take effect; got description %q", got.Description)
	}
	if len(got.Rules) != 1 || got.Rules[0].Access != AccessR {
		t.Errorf("override rules wrong; got %+v", got.Rules)
	}
}

// TestLoadPolicyDirPresetIsVisibleToToolInSameDir — critical invariant
// from the architecture: operator can define a preset AND a tool
// policy that references it, both in the same policy.d, and it works
// because presets are loaded before tool policies.
func TestLoadPolicyDirPresetIsVisibleToToolInSameDir(t *testing.T) {
	dir := t.TempDir()
	const presetName = "test-visible-preset"
	writePolicyFile(t, filepath.Join(dir, "_presets", presetName+".toml"), `
name = "`+presetName+`"
paths = ["/tmp:rw"]
`)
	writePolicyFile(t, filepath.Join(dir, "mytool.toml"), `
name = "mytool"
presets = ["`+presetName+`"]
`)

	t.Cleanup(func() {
		presetRegistry.mu.Lock()
		delete(presetRegistry.presets, presetName)
		presetRegistry.mu.Unlock()
	})

	result, err := LoadPolicyDir(dir, LoadOptions{})
	if err != nil {
		t.Fatalf("preset-before-tool ordering broke: %v", err)
	}
	p, ok := result.Policies["mytool"]
	if !ok {
		t.Fatal("mytool policy not loaded")
	}
	if !slices.Contains(p.AllowedPaths, "/tmp") {
		t.Errorf("preset's /tmp should be in tool's AllowedPaths; got %v", p.AllowedPaths)
	}
}

func TestLoadPolicyDirRejectsNonDirectory(t *testing.T) {
	t.Parallel()
	f, err := os.CreateTemp("", "not-a-dir")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { _ = os.Remove(f.Name()) })

	_, err = LoadPolicyDir(f.Name(), LoadOptions{})
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("expected not-a-directory error, got %v", err)
	}
}

// TestLoadPolicyDirRejectsGroupWritableFile confirms the integrity
// check skips files that don't meet the required mode mask.
// Unix-only (Windows has no Unix mode bits — checkPolicyFilePerms
// on Windows is a no-op warn, tested separately).
func TestLoadPolicyDirRejectsGroupWritableFile(t *testing.T) {
	if runtime_GOOS() == "windows" {
		t.Skip("mode-bit checks don't apply on Windows")
	}
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "git.toml")
	writePolicyFile(t, path, `
name = "git"
paths = ["/tmp:rw"]
`)
	if err := os.Chmod(path, 0o666); err != nil {
		t.Skipf("chmod failed (maybe tmpfs): %v", err)
	}

	result, err := LoadPolicyDir(dir, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.Policies["git"]; ok {
		t.Error("SECURITY: group-writable file should have been rejected")
	}
	if !slices.Contains(result.Rejected, "git") {
		t.Errorf("expected 'git' in Rejected, got %v", result.Rejected)
	}
}

// TestLoadPolicyDirSkipPermChecksAcceptsAnyFile covers the opt-out
// path — deployments on environments where Unix mode semantics
// aren't reliable (some k8s volume drivers, tmpfs mounts with
// non-standard options) need a way to turn the check off without
// having to decompose LoadOptions' individual knobs.
func TestLoadPolicyDirSkipPermChecksAcceptsAnyFile(t *testing.T) {
	if runtime_GOOS() == "windows" {
		t.Skip("mode-bit checks don't apply on Windows")
	}
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "git.toml")
	writePolicyFile(t, path, `
name = "git"
paths = ["/tmp:rw"]
`)
	_ = os.Chmod(path, 0o666)

	result, err := LoadPolicyDir(dir, LoadOptions{SkipPermChecks: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.Policies["git"]; !ok {
		t.Error("SkipPermChecks=true should load the policy despite bad mode")
	}
}

// TestLoadPolicyDirRejectsWrongUID guards the UID check by setting
// an impossible trusted UID (math.MaxInt32 — no real file owner).
// The test file is owned by the test-runner UID, so this must fail.
func TestLoadPolicyDirRejectsWrongUID(t *testing.T) {
	if runtime_GOOS() == "windows" {
		t.Skip("UID checks don't apply on Windows")
	}
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "git.toml")
	writePolicyFile(t, path, `
name = "git"
paths = ["/tmp:rw"]
`)
	result, err := LoadPolicyDir(dir, LoadOptions{TrustedUID: 2147483647}) // MaxInt32 UID
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.Policies["git"]; ok {
		t.Error("SECURITY: wrong-UID file should have been rejected")
	}
	if !slices.Contains(result.Rejected, "git") {
		t.Errorf("expected 'git' in Rejected, got %v", result.Rejected)
	}
}

// TestLoadPolicyDirRejectionIsPerFileNotFatal ensures one bad file
// doesn't poison the whole load — sibling good files still surface.
func TestLoadPolicyDirRejectionIsPerFileNotFatal(t *testing.T) {
	if runtime_GOOS() == "windows" {
		t.Skip("mode checks off on Windows")
	}
	t.Parallel()
	dir := t.TempDir()
	good := filepath.Join(dir, "good.toml")
	bad := filepath.Join(dir, "bad.toml")
	writePolicyFile(t, good, `name = "good"`+"\n"+`paths = ["/tmp:rw"]`)
	writePolicyFile(t, bad, `name = "bad"`+"\n"+`paths = ["/tmp:rw"]`)
	_ = os.Chmod(bad, 0o666)

	result, err := LoadPolicyDir(dir, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.Policies["good"]; !ok {
		t.Error("good file should still load when sibling is rejected")
	}
	if _, ok := result.Policies["bad"]; ok {
		t.Error("bad file should not have loaded")
	}
}

func runtime_GOOS() string { return runtime.GOOS }

func TestLoadPolicyDirIgnoresUnderscorePrefixedFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePolicyFile(t, filepath.Join(dir, "_ignored.toml"), `
name = "_ignored"
paths = ["/tmp:rw"]
`)
	writePolicyFile(t, filepath.Join(dir, "git.toml"), `
name = "git"
paths = ["/tmp:rw"]
`)

	result, err := LoadPolicyDir(dir, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.Policies["_ignored"]; ok {
		t.Error("_ignored.toml should not be loaded as a tool policy")
	}
	if _, ok := result.Policies["git"]; !ok {
		t.Error("git.toml should still be loaded")
	}
}
