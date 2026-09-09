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

	"log/slog"

	"github.com/jmylchreest/lobslaw/internal/promptguard"
)

// Runtime enumerates the skill handler runtimes. MVP supports
// python + bash; go + wasm are roadmap-only.
type Runtime string

const (
	RuntimePython Runtime = "python"
	RuntimeBash   Runtime = "bash"

	// RuntimeProse is a skill with no code: the body IS the skill.
	//
	// Every manifest before this one had to name a handler, which
	// encoded an assumption that turns out to be wrong — that a skill
	// is a program. Most of what the agent teaches itself is procedure
	// in prose: how to approach a class of task, what this user wants,
	// what to check before answering. There is nothing to execute, and
	// inventing a no-op handler so the type-check passes would be a lie
	// that the invoker would then try to run.
	//
	// A prose skill is delivered the way every skill's REFERENCES
	// already are: the index advertises it, the model reads the file.
	// That path exists and works; this only stops the manifest
	// insisting on a handler that was never going to be called.
	RuntimeProse Runtime = "prose"
)

// IsValid reports whether the runtime has a registered executor.
// Operator-facing manifests with unknown runtimes fail Parse so
// typos surface at load time rather than on first invocation.
func (r Runtime) IsValid() bool {
	return r == RuntimePython || r == RuntimeBash || r == RuntimeProse
}

// Executable reports whether the runtime has anything to run.
//
// Asked as a question about the runtime rather than as "is Handler
// empty", so the two can never disagree: validateManifest refuses a
// prose manifest that names a handler and refuses any other manifest
// that omits one.
func (r Runtime) Executable() bool { return r != RuntimeProse }

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

// DefaultVersion stands in for a manifest that declares none, e.g.
// SKILL.md frontmatter has no version field. A fixed literal rather
// than a content digest: the local signing path signs raw manifest
// bytes, so a version that moved with every edit would break every
// signature.
const DefaultVersion = "0.0.0"

// Manifest is the on-disk shape of skills/<name>/manifest.yaml.
// Versioning follows semver; the registry prefers the highest
// version when two mounts expose the same skill name.
type Manifest struct {
	Name        string  `yaml:"name"`
	Version     string  `yaml:"version"`
	Description string  `yaml:"description,omitempty"`
	Runtime     Runtime `yaml:"runtime"`
	Handler     string  `yaml:"handler,omitempty"` // relative to manifest dir
	// HandlerSHA256 is the hex SHA-256 of the handler file, and is
	// what makes a manifest signature worth anything. The signature
	// covers these bytes, so declaring the digest here transitively
	// covers the code: without it a publisher signs a document that
	// merely names a script, and swapping the script afterwards
	// leaves the signature perfectly valid.
	//
	// Required whenever a manifest is signed. Optional otherwise, but
	// honoured if present — a declared digest that does not match is
	// tampering regardless of the signing policy.
	HandlerSHA256 string `yaml:"handler_sha256,omitempty"`

	// Body names a prose document describing HOW to use this skill —
	// the level-1 disclosure, read only when the agent has decided the
	// skill is relevant.
	//
	// A file rather than a manifest field: the description is one line
	// and belongs in the index, whereas this is paragraphs and would
	// make every manifest a document. Relative to the manifest
	// directory; "SKILL.md" is the convention.
	Body string `yaml:"body,omitempty"`

	// BodySHA256 pins it, for the same reason HandlerSHA256 pins the
	// handler and references carry digests: a signature over a
	// manifest that merely NAMES a document leaves the document
	// swappable, and a skill's instructions are as able to change its
	// behaviour as its code is.
	BodySHA256       string             `yaml:"body_sha256,omitempty"`
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
	RequiresBinary []string `yaml:"requires_binary,omitempty"`

	// RequiresCapability names provider capabilities the skill needs
	// (e.g. "vision", "audio"). A skill that summarises screenshots on
	// a text-only deployment is not a broken skill, it is an
	// inapplicable one — and the difference matters, because the index
	// is read on every turn. Listing it teaches the model it has a
	// capability it will then fail to use.
	RequiresCapability []string `yaml:"requires_capability,omitempty"`

	// Platforms restricts the skill to specific GOOS values. Empty
	// means every platform. A skill shelling out to `launchctl` is
	// noise on Linux.
	Platforms []string `yaml:"platforms,omitempty"`

	// References are the bundled documents the skill can be asked
	// about — reference material, templates, scripts. Declared here so
	// the index can say WHAT is available without reading any of it,
	// which is the whole point of disclosing progressively.
	//
	// Each may pin a digest. Pinning the handler and not these leaves
	// a real hole: a skill whose behaviour is driven by an adjacent
	// data file — a prompt template, a rules document — is as
	// changeable as its code, and the signature would still verify.
	References []Reference `yaml:"references,omitempty"`

	Params map[string]any `yaml:"params_schema,omitempty"`
}

// Reference is one bundled document, optionally pinned.
//
// Accepts two YAML shapes so an unsigned skill can declare what it
// carries without computing digests, while a signed one must pin
// everything:
//
//	references:
//	  - references/quick.md                    # declared, unpinned
//	  - path: references/api.md                # pinned
//	    sha256: 3b1f...
type Reference struct {
	Path   string `yaml:"path"`
	SHA256 string `yaml:"sha256,omitempty"`
}

// UnmarshalYAML accepts either a bare path or a mapping.
//
// The bare form is not a convenience to be deprecated later: a skill
// under SigningOff has nothing to sign against, and forcing it to
// carry digests it cannot verify would be ceremony. What matters is
// that the signed path demands them.
func (r *Reference) UnmarshalYAML(unmarshal func(any) error) error {
	var path string
	if err := unmarshal(&path); err == nil {
		r.Path = path
		return nil
	}
	// Alias avoids recursing back into this method.
	type reference Reference
	var full reference
	if err := unmarshal(&full); err != nil {
		return fmt.Errorf("manifest.references: entry must be a path or {path, sha256}: %w", err)
	}
	*r = Reference(full)
	return nil
}

// ReferencePaths projects the declared paths, for the skill index —
// which names what a skill carries without reading any of it.
func ReferencePaths(refs []Reference) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Path)
	}
	return out
}

// MaxDescriptionChars bounds a skill's one-line description.
//
// Enforced at parse rather than at render. Truncating when the index
// is built means an operator writes a 200-character description, sees
// it accepted, and silently loses most of it — the error belongs where
// it can be fixed. The bound exists because the index is rendered on
// every turn and is O(skills): one verbose entry taxes every
// conversation the deployment ever has.
const MaxDescriptionChars = 160

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

	// BodySHA256 is the verified digest of the body document as it was
	// at parse time, empty when none was declared.
	BodySHA256 string

	// HandlerSHA256 is the verified digest of the handler as it was
	// on disk at parse time, empty when the manifest declared none.
	// The invoker re-hashes against this immediately before exec, so
	// a handler swapped after registration is caught.
	HandlerSHA256 string

	// ReferenceSHA256 maps each pinned reference path to its verified
	// digest. Re-checked before exec for the same reason the handler
	// is: a skill driven by an adjacent rules document is as
	// changeable as one driven by its code.
	ReferenceSHA256 map[string]string

	// IsSigned is true iff a valid ed25519 signature by a trusted
	// publisher accompanied the manifest. Under SigningOff this is
	// always false (we never verify); under SigningPrefer /
	// SigningRequire it reflects the actual verification outcome.
	IsSigned bool

	// SignedBy is the operator-assigned name of the key that signed
	// this manifest. Empty when IsSigned is false.
	SignedBy string

	// Tier is where this skill came from, and decides who wins a
	// contested name. Left unset by Parse and derived from IsSigned;
	// set explicitly to TierAgent by whatever materialises the
	// self-taught store, because a parsed manifest carries no trace of
	// having been machine-written.
	Tier SkillTier
}

// Name returns the skill's name. Convenience for registry callers.
func (s *Skill) Name() string { return s.Manifest.Name }

// fileDigest returns the hex SHA-256 of a file's contents, streamed
// rather than read whole — handlers are small, but this is also the
// path a bundled binary would take.
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Parse reads manifest.yaml from dir without signature checks. Kept
// as the ergonomic default for tests and for deployments running
// SigningOff. For signature-aware parsing use ParseWithPolicy.
func Parse(dir string) (*Skill, error) {
	return ParseWithPolicy(dir, SigningOff, nil)
}

// ParseAgentSkill parses a skill the agent wrote for itself.
//
// The door for materialised self-taught artefacts. Two things it does
// that Parse does not: it tags the skill TierAgent, because a parsed
// manifest carries no trace of having been machine-written and
// provenance-by-location is what establishes it; and it applies the
// capability floor, so a skill that asks to widen the deployment's
// surface fails to load with the reason rather than loading with less
// than it declared.
//
// Signing is off for this path by construction — an agent-authored
// manifest has no publisher, and pretending to verify one would be
// theatre.
func ParseAgentSkill(dir string) (*Skill, error) {
	skill, err := ParseWithPolicy(dir, SigningOff, nil)
	if err != nil {
		return nil, err
	}
	if err := checkAgentFloor(&skill.Manifest); err != nil {
		return nil, err
	}
	skill.Tier = TierAgent
	return skill, nil
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

	// A prose skill has no handler to resolve, so HandlerPath stays
	// empty rather than pointing at the manifest directory. An empty
	// string is a value the invoker cannot mistake for a script;
	// dir-as-handler is one it could try to exec.
	var handler string
	if m.Runtime.Executable() {
		handler = filepath.Join(dir, m.Handler)
		if _, err := os.Stat(handler); err != nil {
			return nil, fmt.Errorf("skills: handler %q: %w", handler, err)
		}
	}

	sum := sha256.Sum256(raw)
	// The description is prose that reaches the model — it is how a
	// skill advertises itself — so it is worth the same scan as any
	// other text bound for a prompt. Warn rather than reject: a
	// signature already covers provenance, and refusing to load a
	// skill on a heuristic would break a working install over a
	// phrase.
	for _, f := range promptguard.Scan(m.Name + "\n" + m.Description) {
		slog.Default().Warn("skills: suspicious content in manifest",
			"skill", m.Name, "dir", dir, "detector", f.Detector, "detail", f.Detail)
	}

	skill := &Skill{
		// Underived, not TierAgent — the zero value is the lowest tier
		// and would make every parsed skill lose to everything.
		Tier:        tierUnset,
		Manifest:    m,
		ManifestDir: dir,
		HandlerPath: handler,
		SHA256:      hex.EncodeToString(sum[:]),
	}

	// A declared handler digest is checked whatever the signing
	// policy says. The policy governs whether we demand provenance,
	// not whether we believe a digest the manifest itself states.
	if declared := strings.TrimSpace(m.HandlerSHA256); declared != "" {
		actual, err := fileDigest(handler)
		if err != nil {
			return nil, fmt.Errorf("skills: hash handler %q: %w", handler, err)
		}
		if !strings.EqualFold(actual, declared) {
			return nil, fmt.Errorf("skills: handler %q does not match manifest handler_sha256 "+
				"(declared %s, found %s)", handler, declared, actual)
		}
		skill.HandlerSHA256 = actual
	}

	if err := verifyBody(dir, m, skill); err != nil {
		return nil, err
	}

	refDigests, err := verifyReferences(dir, m.References)
	if err != nil {
		return nil, err
	}
	skill.ReferenceSHA256 = refDigests

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
	// A verified signature over a manifest that does not pin its
	// handler is worse than no signature: it reads as provenance in
	// logs and in the registry's signed-candidate preference, while
	// covering nothing that executes. Refuse it rather than record a
	// guarantee we cannot make.
	if skill.HandlerSHA256 == "" {
		return nil, fmt.Errorf("skills: %q: signed manifest does not declare handler_sha256, "+
			"so the signature covers no executable content; re-publish with the handler digest pinned", manifestPath)
	}
	// And the same for the body, for the same reason. A skill's
	// instructions steer what the agent does with it as surely as its
	// code does — a signature covering a manifest that merely NAMES a
	// document leaves the document swappable, and the swap would not
	// break the signature.
	if strings.TrimSpace(m.Body) != "" && strings.TrimSpace(m.BodySHA256) == "" {
		return nil, fmt.Errorf("skills: %q: signed manifest declares a body but no body_sha256, "+
			"so the signature does not cover the instructions; re-publish with the body digest pinned",
			manifestPath)
	}
	// Same argument one level out. A skill whose behaviour comes from
	// an adjacent rules document is as changeable as one whose
	// behaviour comes from its code, and a signature that covers the
	// code and not the document reads as provenance while guaranteeing
	// less than it appears to.
	for _, r := range m.References {
		if strings.TrimSpace(r.SHA256) == "" {
			return nil, fmt.Errorf(
				"skills: %q: signed manifest declares reference %q with no sha256; "+
					"the signature would not cover it — re-publish with every reference pinned",
				manifestPath, r.Path)
		}
	}
	skill.IsSigned = true
	skill.SignedBy = signer
	return skill, nil
}

// validateManifest applies the manifest-shape invariants. Listed
// in a single place so Parse and test code share the checks.
// validateDisclosure checks the fields that only exist to make the
// skill index cheap and honest: the one-line description, the gating
// that keeps inapplicable skills out of it, and the reference paths it
// advertises.
//
// Split out of validateManifest because that function is a long list
// of unrelated field checks and adding a sixth concern to it made the
// complexity linter right rather than pedantic.
// verifyReferences hashes every pinned reference and returns the
// verified digests.
//
// A declared digest is checked whatever the signing policy says, for
// the same reason the handler's is: the policy governs whether we
// demand provenance, not whether we believe a digest the manifest
// itself states.
func verifyReferences(dir string, refs []Reference) (map[string]string, error) {
	var out map[string]string
	for _, r := range refs {
		declared := strings.TrimSpace(r.SHA256)
		if declared == "" {
			continue
		}
		path := filepath.Join(dir, filepath.Clean(r.Path))
		actual, err := fileDigest(path)
		if err != nil {
			return nil, fmt.Errorf("skills: hash reference %q: %w", r.Path, err)
		}
		if !strings.EqualFold(actual, declared) {
			return nil, fmt.Errorf("skills: reference %q does not match its declared sha256 "+
				"(declared %s, found %s)", r.Path, declared, actual)
		}
		if out == nil {
			out = map[string]string{}
		}
		out[r.Path] = actual
	}
	return out, nil
}

// validateBinaries checks the bundled-binary declarations. Split out
// for the same reason as validateDisclosure: validateManifest was a
// flat list of unrelated field checks sitting at the complexity
// ceiling, so the next addition to it had to pay for a seam.
func validateBinaries(m *Manifest) error {
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
	return nil
}

// validateHandler enforces the handler rules for the manifest's
// runtime.
//
// Symmetrical on purpose. An executable runtime must name a handler,
// as it always had to; a prose runtime must NOT — a manifest that says
// "there is nothing to run" while pointing at a script is a manifest
// whose two halves disagree, and the half that wins would be decided
// by whichever code path read it first.
//
// handler_sha256 goes with the handler. A digest pinning a file that
// does not exist is not harmless ceremony: it reads, to anybody
// auditing, as a skill whose code is pinned.
func validateHandler(m *Manifest, dir string) error {
	if !m.Runtime.Executable() {
		if m.Handler != "" {
			return fmt.Errorf(
				"manifest.handler %q is set but runtime is %q, which runs nothing — "+
					"drop the handler, or name a runtime that executes it", m.Handler, m.Runtime)
		}
		if m.HandlerSHA256 != "" {
			return fmt.Errorf("manifest.handler_sha256 is set but runtime %q has no handler", m.Runtime)
		}
		return nil
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
	return nil
}

func validateDisclosure(m *Manifest) error {
	if n := len([]rune(m.Description)); n > MaxDescriptionChars {
		return fmt.Errorf(
			"manifest.description is %d characters, limit is %d — it is rendered in the "+
				"skill index on every turn, so put the detail in the skill body instead",
			n, MaxDescriptionChars)
	}
	if strings.ContainsAny(m.Description, "\n\r") {
		return errors.New("manifest.description must be a single line; the index renders one entry per skill")
	}
	for i, p := range m.Platforms {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("manifest.platforms[%d] is empty", i)
		}
	}
	for i, c := range m.RequiresCapability {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("manifest.requires_capability[%d] is empty", i)
		}
	}
	for i, r := range m.References {
		if strings.TrimSpace(r.Path) == "" {
			return fmt.Errorf("manifest.references[%d].path is required", i)
		}
		cleaned := filepath.Clean(r.Path)
		if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
			return fmt.Errorf(
				"manifest.references[%d] %q must be relative and inside the skill directory", i, r.Path)
		}
	}
	return nil
}

func validateManifest(m *Manifest, dir string) error {
	if m.Name == "" {
		return errors.New("manifest.name is required")
	}
	if strings.ContainsAny(m.Name, "/\\") {
		return fmt.Errorf("manifest.name %q must not contain path separators", m.Name)
	}
	if m.Version == "" {
		// Defaults the struct only; raw bytes and any signature are
		// already captured by the caller and stay untouched.
		m.Version = DefaultVersion
	}
	if err := validateDisclosure(m); err != nil {
		return err
	}
	if !m.Runtime.IsValid() {
		return fmt.Errorf("manifest.runtime %q unsupported (python, bash, prose)", m.Runtime)
	}
	if err := validateHandler(m, dir); err != nil {
		return err
	}
	if err := validateBinaries(m); err != nil {
		return err
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

// verifyBody checks the declared body document.
//
// A declared body that is MISSING is an error rather than an empty
// one: the manifest promised documentation the agent will ask for, and
// answering "there is none" for a skill that says otherwise sends
// somebody looking for a bug in the wrong place.
//
// A declared digest that does not match is tampering regardless of the
// signing policy — the same rule the handler follows, for the same
// reason. A skill's instructions can change what it does as surely as
// its code can.
func verifyBody(dir string, m Manifest, skill *Skill) error {
	rel := strings.TrimSpace(m.Body)
	if rel == "" {
		return nil
	}
	path := filepath.Join(dir, filepath.Clean(rel))
	declared := strings.TrimSpace(m.BodySHA256)
	actual, err := fileDigest(path)
	if err != nil {
		return fmt.Errorf("skills: body %q: %w", rel, err)
	}
	if declared != "" && !strings.EqualFold(actual, declared) {
		return fmt.Errorf("skills: body %q does not match its declared sha256 "+
			"(declared %s, found %s)", rel, declared, actual)
	}
	skill.BodySHA256 = actual
	return nil
}
