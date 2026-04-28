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
//
// Subpath narrows the access to a sub-directory under the mount
// root. This is what lets multiple clawhub-installed skills share
// one operator-declared mount: each skill claims a different
// subpath under the shared "skill-tools" + "skill-data" labels.
// Empty Subpath grants the full mount root (legacy behaviour).
//
// Example manifest fragment:
//
//	storage:
//	  - { label: skill-tools, subpath: gws-workspace, mode: read }
//	  - { label: skill-data,  subpath: gws-workspace, mode: write }
type StorageAccess struct {
	Label   string      `yaml:"label"`
	Subpath string      `yaml:"subpath,omitempty"`
	Mode    StorageMode `yaml:"mode,omitempty"` // default: read
}

// Manifest is the on-disk shape of skills/<name>/manifest.yaml.
// Versioning follows semver; the registry prefers the highest
// version when two mounts expose the same skill name.
type Manifest struct {
	Name             string             `yaml:"name"`
	Version          string             `yaml:"version"`
	Description      string             `yaml:"description,omitempty"`
	Runtime          Runtime            `yaml:"runtime"`
	Handler          string             `yaml:"handler"` // relative to manifest dir
	Storage          []StorageAccess    `yaml:"storage,omitempty"`
	Network          []string           `yaml:"network,omitempty"`
	NetworkIsolation bool               `yaml:"network_isolation,omitempty"`
	NetworkAllowDNS  bool               `yaml:"network_allow_dns,omitempty"`
	Credentials      []CredentialAccess `yaml:"credentials,omitempty"`
	Binaries         []BinaryAccess     `yaml:"binaries,omitempty"`
	// RequiresBinary names host-level binaries that must resolve in
	// PATH before the skill is invoked. Distinct from Binaries
	// (which ships bundle-internal binaries via clawhub install).
	// RequiresBinary entries are typically satisfied by the skill's
	// own clawdbot.install array (parsed by the clawhub install
	// pipeline), or by pre-installation on the host. The invoker
	// runs LookPath against each name pre-spawn; if any are missing
	// it returns a structured error.
	RequiresBinary   []string           `yaml:"requires_binary,omitempty"`
	Params           map[string]any     `yaml:"params_schema,omitempty"`
}

// BinaryAccess declares one binary the skill bundles. The install
// pipeline fetches each binary (Phase B), verifies SHA-256 against
// the manifest's declared digest, and writes it under the install
// dir at the named Target with the executable bit set. Hosting URL
// must resolve to a host inside the egress "clawhub" role's
// allowlist (e.g. github.com release endpoints).
//
// Binaries declared in the manifest are part of the signed bundle:
// any change to URL/SHA/Target invalidates the publisher signature.
//
// Example manifest fragment:
//
//	binaries:
//	  - name: gws-cli
//	    url: https://github.com/myorg/gws-cli/releases/download/v1.0.0/gws-cli
//	    sha256: a1b2c3...
//	    target: bin/gws-cli
type BinaryAccess struct {
	Name   string `yaml:"name"`
	URL    string `yaml:"url"`
	SHA256 string `yaml:"sha256"`
	Target string `yaml:"target"`
}

// CredentialAccess declares one credential a skill needs at invocation
// time. The invoker resolves (provider, subject) via the credential
// service, validates the per-skill ACL, refreshes the token if near
// expiry, and injects the access token via env. Subject is optional
// in single-user setups — when omitted the invoker requires exactly
// one credential bound to the provider; multiple matches abort the
// invocation with an "ambiguous subject" error.
//
// Example manifest fragment:
//
//	credentials:
//	  - { provider: google, subject: alice@example.com }
//	  - { provider: github }
type CredentialAccess struct {
	Provider string `yaml:"provider"`
	Subject  string `yaml:"subject,omitempty"`
}

// Skill is the registered form — manifest + resolved disk paths +
// SHA of the manifest file + signature-verification result. SHA
// lets the registry detect content-identical republishes (no event)
// vs actual changes (reload + notify subscribers). IsSigned +
// SignedBy let the registry prefer signed candidates during
// winner-selection and audit logs show who signed what.
type Skill struct {
	Manifest    Manifest
	ManifestDir string // absolute path to the directory containing manifest.yaml
	HandlerPath string // absolute path to the handler script
	SHA256      string // hex-encoded manifest-file digest

	// IsSigned is true iff a valid ed25519 signature by a trusted
	// publisher accompanied the manifest. Under SigningOff this is
	// always false (we never verify); under SigningPrefer /
	// SigningRequire it reflects the actual verification outcome.
	IsSigned bool

	// SignedBy is the operator-assigned name of the key that signed
	// this manifest. Empty when IsSigned is false.
	SignedBy string
}

// Name returns the skill's name. Convenience for registry callers.
func (s *Skill) Name() string { return s.Manifest.Name }

// Parse reads manifest.yaml from dir without signature checks. Kept
// as the ergonomic default for tests and for deployments running
// SigningOff. For signature-aware parsing use ParseWithPolicy.
func Parse(dir string) (*Skill, error) {
	return ParseWithPolicy(dir, SigningOff, nil)
}

// ParseWithPolicy is the production entry point. SigningOff ignores
// signatures. SigningPrefer verifies when present — missing is
// fine, invalid rejects (indicates tampering / broken publish).
// SigningRequire rejects both missing and invalid. verifier may be
// nil only under SigningOff.
func ParseWithPolicy(dir string, policy SigningPolicy, verifier *Verifier) (*Skill, error) {
	if !policy.IsValid() {
		return nil, fmt.Errorf("skills: unsupported signing policy %q", policy)
	}
	if policy != SigningOff && verifier == nil {
		return nil, fmt.Errorf("skills: policy %q requires a Verifier", policy)
	}

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
	skill := &Skill{
		Manifest:    m,
		ManifestDir: dir,
		HandlerPath: handler,
		SHA256:      hex.EncodeToString(sum[:]),
	}

	if policy == SigningOff {
		return skill, nil
	}

	sig, err := readSignature(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("skills: %q: %w", manifestPath, err)
	}

	if sig == nil {
		if policy == SigningRequire {
			return nil, fmt.Errorf("skills: %q: signature required but manifest.yaml.sig missing", manifestPath)
		}
		return skill, nil
	}

	signer, ok := verifier.Verify(raw, sig)
	if !ok {
		return nil, fmt.Errorf("skills: %q: signature present but did not verify against any trusted key", manifestPath)
	}
	skill.IsSigned = true
	skill.SignedBy = signer
	return skill, nil
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
	for i, b := range m.Binaries {
		if strings.TrimSpace(b.Name) == "" {
			return fmt.Errorf("manifest.binaries[%d].name is required", i)
		}
		if strings.TrimSpace(b.URL) == "" {
			return fmt.Errorf("manifest.binaries[%d].url is required", i)
		}
		if strings.TrimSpace(b.SHA256) == "" {
			return fmt.Errorf("manifest.binaries[%d].sha256 is required", i)
		}
		if strings.TrimSpace(b.Target) == "" {
			return fmt.Errorf("manifest.binaries[%d].target is required", i)
		}
		cleaned := filepath.Clean(b.Target)
		if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, "/../") {
			return fmt.Errorf("manifest.binaries[%d].target %q must be relative and not escape the install dir", i, b.Target)
		}
	}
	for i, c := range m.Credentials {
		if strings.TrimSpace(c.Provider) == "" {
			return fmt.Errorf("manifest.credentials[%d].provider is required", i)
		}
		if strings.ContainsAny(c.Provider, ":/") {
			return fmt.Errorf("manifest.credentials[%d].provider %q must not contain ':' or '/'", i, c.Provider)
		}
		if strings.Contains(c.Subject, ":") {
			return fmt.Errorf("manifest.credentials[%d].subject %q must not contain ':'", i, c.Subject)
		}
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
		if sp := m.Storage[i].Subpath; sp != "" {
			// Subpath is appended under the mount root by the
			// resolver; reject traversal attempts at parse time so
			// a malicious manifest can't smuggle "../etc" past
			// the resolver's check via odd encodings.
			cleaned := filepath.Clean(sp)
			if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") || filepath.IsAbs(cleaned) {
				return fmt.Errorf("manifest.storage[%d].subpath %q must be relative and not escape the mount root", i, sp)
			}
		}
	}
	return nil
}
