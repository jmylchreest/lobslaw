package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	koanftoml "github.com/knadh/koanf/parsers/toml/v2"
	koanffile "github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

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
// tool-name → *Policy for every tool .toml found, plus a count of
// preset files loaded (useful for startup logs).
type LoadResult struct {
	Policies       map[string]*Policy
	PresetsLoaded  []string
	OverriddenBuiltins []string
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
func LoadPolicyDir(dir string) (*LoadResult, error) {
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
		if err := loadPresetsFromDir(presetsDir, result); err != nil {
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
		policy, err := loadToolPolicyFile(path, name)
		if err != nil {
			return nil, err
		}
		result.Policies[name] = policy
	}

	return result, nil
}

// loadPresetsFromDir reads every .toml under the presets dir and
// registers each one. Collisions with built-ins are recorded in the
// result so callers can log them — shadowing is allowed but should
// be visible.
func loadPresetsFromDir(dir string, result *LoadResult) error {
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
		spec, err := loadPresetFile(path, name)
		if err != nil {
			return err
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

// loadToolPolicyFile parses a single policy.d/<tool>.toml file. The
// filename (without .toml) is the canonical tool name; if the file's
// `name` field is set, it must match.
func loadToolPolicyFile(path, expectName string) (*Policy, error) {
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
// Same filename-match invariant as tool policies.
func loadPresetFile(path, expectName string) (*PresetSpec, error) {
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
