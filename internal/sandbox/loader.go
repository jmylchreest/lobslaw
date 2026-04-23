package sandbox

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	koanftoml "github.com/knadh/koanf/parsers/toml/v2"
	koanffile "github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// LoadOptions tunes LoadPolicyDir's integrity checks. Zero value
// is safe: the agent's own UID is trusted, group/world-writable
// files are rejected. For the "I know what I'm doing, disable
// checks entirely" case (some k8s volume drivers, test scaffolding)
// set SkipPermChecks = true.
type LoadOptions struct {
	// TrustedUID is the UID whose files are accepted. Defaults to
	// os.Geteuid() on Unix (the agent's own UID — "I only trust
	// files I wrote or someone with my privileges did"). Set to a
	// negative value to skip the UID check while still enforcing
	// the mode mask. Ignored on Windows (see permcheck_windows.go).
	TrustedUID int

	// RejectWritableMask is bitwise-AND'd against the file's Unix
	// mode bits; any match rejects the file. Defaults to 0022
	// (group-write or other-write).
	RejectWritableMask fs.FileMode

	// SkipPermChecks disables the integrity check entirely — no
	// mode, no UID. Use when the deployment environment can't
	// guarantee Unix mode semantics (e.g. Windows, some k8s
	// volume mounts). The sandbox enforcement itself is already
	// no-op on non-Linux, so this isn't a further loss of defence
	// on those platforms.
	SkipPermChecks bool

	// Logger is used for warnings ("shadowing builtin preset X",
	// "rejected file Y because …"). Defaults to slog.Default().
	Logger *slog.Logger
}

// withDefaults returns a copy of o with zero-value fields filled in
// with the safe defaults — called once at the top of LoadPolicyDir
// so every downstream helper sees the same resolved values.
func (o LoadOptions) withDefaults() LoadOptions {
	out := o
	if out.TrustedUID == 0 {
		out.TrustedUID = defaultTrustedUID()
	}
	if out.RejectWritableMask == 0 {
		out.RejectWritableMask = 0o022
	}
	if out.Logger == nil {
		out.Logger = slog.Default()
	}
	return out
}

// DirLayout describes the conventional layout of a policy.d/ dir:
//
//	policy.d/
//	├── git.toml           # tool policy: name == filename stem
//	├── rsync.toml
//	├── _presets/          # optional operator preset overrides
//	│   ├── my-code.toml   # preset: name == filename stem
//	│   └── corp-certs.toml
//	└── ...
//
// The `_presets/` prefix is a filesystem convention — the loader
// treats anything under that subdir as PresetSpec (registers via
// RegisterPreset, shadowing built-ins on name collision). Anything
// else at the top level of the directory is a PolicySpec.
const PresetSubdir = "_presets"

// LoadResult is the aggregate output of LoadPolicyDir — a map of
// tool-name → *Policy for every tool .toml found, plus a list of
// presets loaded, any built-ins shadowed, and any files rejected by
// the integrity check (useful for startup logs).
type LoadResult struct {
	Policies           map[string]*Policy
	PresetsLoaded      []string
	OverriddenBuiltins []string
	// Rejected lists files that failed the perm/UID check. Names
	// are bare tool names for top-level files, and "_presets/<name>"
	// for preset files, so operators can grep startup logs and see
	// which file triggered the rejection.
	Rejected []string
}

// LoadPolicyDirs is the multi-directory variant of LoadPolicyDir.
// Each directory is loaded in order; the resulting LoadResults are
// merged with "later wins" semantics — the rule the Registry already
// follows via last-write-wins SetPolicy, so precedence at the loader
// matches precedence at the sink.
//
// Merge rules:
//
//   - Policies map: a later tool policy for the same name overwrites
//     the earlier one. If you want "operator overrides user-global,"
//     put operator's dir later in the list.
//   - Preset registrations: later RegisterPreset calls shadow earlier
//     ones (same as single-dir mode, just repeated). OverriddenBuiltins
//     in the final result lists every such shadow across all dirs.
//   - PresetsLoaded / Rejected: unioned (operators get a full
//     accounting of what happened).
//
// Missing directories are no-ops, consistent with LoadPolicyDir.
func LoadPolicyDirs(dirs []string, opts LoadOptions) (*LoadResult, error) {
	merged := &LoadResult{Policies: map[string]*Policy{}}
	for _, dir := range dirs {
		r, err := LoadPolicyDir(dir, opts)
		if err != nil {
			return merged, fmt.Errorf("load policy dir %q: %w", dir, err)
		}
		for name, policy := range r.Policies {
			merged.Policies[name] = policy
		}
		merged.PresetsLoaded = append(merged.PresetsLoaded, r.PresetsLoaded...)
		merged.OverriddenBuiltins = append(merged.OverriddenBuiltins, r.OverriddenBuiltins...)
		merged.Rejected = append(merged.Rejected, r.Rejected...)
	}
	return merged, nil
}

// LoadPolicyDir walks dir and returns all discovered tool policies,
// registering any operator preset overrides along the way. Safe to
// call with a non-existent dir (returns empty result, no error) —
// callers can unconditionally point the loader at the conventional
// path without feature-detecting whether it exists.
//
// Processing order is intentional:
//  1. _presets/*.toml → RegisterPreset (may override built-ins)
//  2. top-level *.toml → PolicySpec → ToPolicy() (may reference the
//     presets loaded in step 1)
//
// So operator-supplied preset overrides are visible to operator-
// supplied tool policies in the same directory.
//
// Each file is integrity-checked before parsing — group/world-writable
// files and files not owned by the trusted UID are rejected with a
// visible warning log (not silent) so operators see tampering
// attempts. See LoadOptions for the knobs.
func LoadPolicyDir(dir string, opts LoadOptions) (*LoadResult, error) {
	opts = opts.withDefaults()

	if dir == "" {
		return &LoadResult{Policies: map[string]*Policy{}}, nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return &LoadResult{Policies: map[string]*Policy{}}, nil
		}
		return nil, fmt.Errorf("stat policy dir %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("policy dir %q is not a directory", dir)
	}

	result := &LoadResult{Policies: map[string]*Policy{}}

	presetsDir := filepath.Join(dir, PresetSubdir)
	if _, err := os.Stat(presetsDir); err == nil {
		if err := loadPresetsFromDir(presetsDir, result, opts); err != nil {
			return nil, err
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read policy dir %q: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".toml") {
			continue
		}
		// Skip anything that starts with `_` — reserved for subdirs
		// (presets) or operator-hidden files.
		if strings.HasPrefix(entry.Name(), "_") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		name := strings.TrimSuffix(entry.Name(), ".toml")
		policy, err := loadToolPolicyFile(path, name, opts)
		if err != nil {
			return nil, err
		}
		// policy is nil when the perm check rejected — skip but keep going.
		if policy == nil {
			result.Rejected = append(result.Rejected, name)
			continue
		}
		result.Policies[name] = policy
	}

	return result, nil
}

// loadPresetsFromDir reads every .toml under the presets dir and
// registers each one. Collisions with built-ins are recorded in the
// result so callers can log them — shadowing is allowed but should
// be visible.
func loadPresetsFromDir(dir string, result *LoadResult, opts LoadOptions) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read presets dir %q: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".toml") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		name := strings.TrimSuffix(entry.Name(), ".toml")
		spec, err := loadPresetFile(path, name, opts)
		if err != nil {
			return err
		}
		if spec == nil {
			result.Rejected = append(result.Rejected, "_presets/"+name)
			continue
		}
		if _, wasBuiltin := LookupPreset(spec.Name); wasBuiltin {
			// Shadowing a built-in — operators should know.
			result.OverriddenBuiltins = append(result.OverriddenBuiltins, spec.Name)
		}
		preset, err := spec.ToPreset()
		if err != nil {
			return err
		}
		RegisterPreset(preset)
		result.PresetsLoaded = append(result.PresetsLoaded, spec.Name)
	}
	return nil
}

// verifyAndLog runs checkPolicyFilePerms and, on rejection, logs at
// Warn level with the path + reason. Centralised so both the preset
// and tool-policy loaders surface rejections uniformly. When
// opts.SkipPermChecks is true the check is bypassed entirely.
func verifyAndLog(path string, opts LoadOptions) bool {
	if opts.SkipPermChecks {
		return true
	}
	info, err := os.Stat(path)
	if err != nil {
		opts.Logger.Warn("sandbox policy: stat failed; skipping",
			"path", path, "error", err)
		return false
	}
	if err := checkPolicyFilePerms(path, info, opts.TrustedUID, opts.RejectWritableMask); err != nil {
		opts.Logger.Warn("sandbox policy: rejected integrity check; skipping",
			"path", path, "reason", err)
		return false
	}
	return true
}

// loadToolPolicyFile parses a single policy.d/<tool>.toml file. The
// filename (without .toml) is the canonical tool name; if the file's
// `name` field is set, it must match. Returns (nil, nil) when the
// perm check rejects the file — caller skips rather than errors.
func loadToolPolicyFile(path, expectName string, opts LoadOptions) (*Policy, error) {
	if !verifyAndLog(path, opts) {
		return nil, nil
	}
	k := koanf.New(".")
	if err := k.Load(koanffile.Provider(path), koanftoml.Parser()); err != nil {
		return nil, fmt.Errorf("load %q: %w", path, err)
	}
	var spec PolicySpec
	if err := k.UnmarshalWithConf("", &spec, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return nil, fmt.Errorf("parse %q: %w", path, err)
	}
	if spec.Name == "" {
		spec.Name = expectName
	} else if spec.Name != expectName {
		return nil, fmt.Errorf("policy file %q: name %q doesn't match filename", path, spec.Name)
	}
	p, err := spec.ToPolicy()
	if err != nil {
		return nil, err
	}
	return p, nil
}

// loadPresetFile parses a single policy.d/_presets/<name>.toml file.
// Same filename-match invariant as tool policies. Returns (nil, nil)
// when the perm check rejects the file.
func loadPresetFile(path, expectName string, opts LoadOptions) (*PresetSpec, error) {
	if !verifyAndLog(path, opts) {
		return nil, nil
	}
	k := koanf.New(".")
	if err := k.Load(koanffile.Provider(path), koanftoml.Parser()); err != nil {
		return nil, fmt.Errorf("load preset %q: %w", path, err)
	}
	var spec PresetSpec
	if err := k.UnmarshalWithConf("", &spec, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return nil, fmt.Errorf("parse preset %q: %w", path, err)
	}
	if spec.Name == "" {
		spec.Name = expectName
	} else if spec.Name != expectName {
		return nil, fmt.Errorf("preset file %q: name %q doesn't match filename", path, spec.Name)
	}
	return &spec, nil
}
