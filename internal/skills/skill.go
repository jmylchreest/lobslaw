package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Runtime enumerates the skill handler runtimes. MVP supports
// python + bash; go + wasm are roadmap-only.
type Runtime string

const (
	RuntimePython Runtime = "python"
	RuntimeBash   Runtime = "bash"
)

// IsValid reports whether the runtime has a registered executor.
// Operator-facing manifests with unknown runtimes fail Parse so
// typos surface at load time rather than on first invocation.
func (r Runtime) IsValid() bool {
	return r == RuntimePython || r == RuntimeBash
}

// StorageMode is read vs. read-write access to a mount.
type StorageMode string

const (
	StorageRead  StorageMode = "read"
	StorageWrite StorageMode = "write"
)

// StorageAccess declares one label the skill requires access to.
// Raw paths are never permitted — operators wire a storage mount
// pointing at the desired path first.
type StorageAccess struct {
	Label string      `yaml:"label"`
	Mode  StorageMode `yaml:"mode,omitempty"` // default: read
}

// Manifest is the on-disk shape of skills/<name>/manifest.yaml.
// Versioning follows semver; the registry prefers the highest
// version when two mounts expose the same skill name.
type Manifest struct {
	Name        string          `yaml:"name"`
	Version     string          `yaml:"version"`
	Description string          `yaml:"description,omitempty"`
	Runtime     Runtime         `yaml:"runtime"`
	Handler     string          `yaml:"handler"`  // relative to manifest dir
	Storage     []StorageAccess `yaml:"storage,omitempty"`
	Network     []string        `yaml:"network,omitempty"`
	Params      map[string]any  `yaml:"params_schema,omitempty"`
}

// Skill is the registered form — manifest + resolved disk paths +
// SHA of the manifest file. SHA lets the registry detect
// content-identical republishes (no event) vs actual changes
// (reload + notify subscribers).
type Skill struct {
	Manifest    Manifest
	ManifestDir string // absolute path to the directory containing manifest.yaml
	HandlerPath string // absolute path to the handler script
	SHA256      string // hex-encoded manifest-file digest
}

// Name returns the skill's name. Convenience for registry callers.
func (s *Skill) Name() string { return s.Manifest.Name }

// Parse reads manifest.yaml from dir, validates the shape, and
// resolves the handler path. Returns an error for any validation
// failure — rather than a partially-populated Skill — so callers
// can't accidentally act on a half-registered skill.
func Parse(dir string) (*Skill, error) {
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("skills: manifest dir %q must be absolute", dir)
	}
	manifestPath := filepath.Join(dir, "manifest.yaml")
	f, err := os.Open(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("skills: open %q: %w", manifestPath, err)
	}
	defer func() { _ = f.Close() }()

	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("skills: read %q: %w", manifestPath, err)
	}

	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("skills: parse %q: %w", manifestPath, err)
	}
	if err := validateManifest(&m, dir); err != nil {
		return nil, fmt.Errorf("skills: %q: %w", manifestPath, err)
	}

	handler := filepath.Join(dir, m.Handler)
	if _, err := os.Stat(handler); err != nil {
		return nil, fmt.Errorf("skills: handler %q: %w", handler, err)
	}

	sum := sha256.Sum256(raw)
	return &Skill{
		Manifest:    m,
		ManifestDir: dir,
		HandlerPath: handler,
		SHA256:      hex.EncodeToString(sum[:]),
	}, nil
}

// validateManifest applies the manifest-shape invariants. Listed
// in a single place so Parse and test code share the checks.
func validateManifest(m *Manifest, dir string) error {
	if m.Name == "" {
		return errors.New("manifest.name is required")
	}
	if strings.ContainsAny(m.Name, "/\\") {
		return fmt.Errorf("manifest.name %q must not contain path separators", m.Name)
	}
	if m.Version == "" {
		return errors.New("manifest.version is required")
	}
	if !m.Runtime.IsValid() {
		return fmt.Errorf("manifest.runtime %q unsupported (python, bash)", m.Runtime)
	}
	if m.Handler == "" {
		return errors.New("manifest.handler is required")
	}
	// The handler must resolve to a path inside the manifest dir —
	// belt + braces against traversal via "../" in operator-authored
	// manifests. Manifests arrive from storage mounts the operator
	// already trusts, but the runtime check costs nothing.
	handlerAbs := filepath.Join(dir, m.Handler)
	rel, err := filepath.Rel(dir, handlerAbs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("manifest.handler %q must be inside the manifest directory", m.Handler)
	}
	for i := range m.Storage {
		if m.Storage[i].Label == "" {
			return fmt.Errorf("manifest.storage[%d].label is required", i)
		}
		if m.Storage[i].Mode == "" {
			m.Storage[i].Mode = StorageRead
		}
		if m.Storage[i].Mode != StorageRead && m.Storage[i].Mode != StorageWrite {
			return fmt.Errorf("manifest.storage[%d].mode %q must be read|write", i, m.Storage[i].Mode)
		}
	}
	return nil
}
