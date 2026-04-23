package skills

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SigningPolicy is the operator-level stance on manifest signatures.
// Keeping it tri-state (rather than a boolean "required?") reflects
// how the ecosystem actually works: most community skills ship
// unsigned; requiring signatures would exclude them entirely, while
// ignoring signatures loses the safety benefit for skills that DO
// ship signed.
type SigningPolicy string

const (
	// SigningOff treats signatures as decoration. Missing / invalid
	// signatures are silently ignored; Parse returns the manifest
	// with IsSigned=false regardless.
	SigningOff SigningPolicy = "off"

	// SigningPrefer accepts both signed and unsigned manifests. When
	// two versions of the same skill name tie, the signed one wins
	// in registry winner-selection. Good default for mixed
	// community-plus-trusted deployments.
	SigningPrefer SigningPolicy = "prefer"

	// SigningRequire rejects any unsigned or invalidly-signed
	// manifest at Parse time. Appropriate for production deployments
	// where every skill must come from a vetted publisher.
	SigningRequire SigningPolicy = "require"
)

// ParseSigningPolicy accepts the three valid strings and maps any
// other value (including "") to SigningPrefer — the safest default
// that doesn't silently downgrade security.
func ParseSigningPolicy(s string) SigningPolicy {
	switch SigningPolicy(strings.ToLower(strings.TrimSpace(s))) {
	case SigningOff:
		return SigningOff
	case SigningRequire:
		return SigningRequire
	default:
		return SigningPrefer
	}
}

// IsValid reports whether p is one of the three supported values.
// Config validators call this so typos like "yes"/"true" fail
// loudly at boot rather than silently defaulting.
func (p SigningPolicy) IsValid() bool {
	return p == SigningOff || p == SigningPrefer || p == SigningRequire
}

// Verifier verifies ed25519 signatures against a pre-shared set of
// public keys. Operators load keys from filesystem paths at boot
// (paths come from config.skills.trusted_publishers_file or an
// inline list). An empty verifier accepts no signatures — with
// SigningRequire that rejects everything, which is the correct
// fail-closed behaviour for "I asked for signing but didn't wire
// keys."
type Verifier struct {
	keys map[string]ed25519.PublicKey
}

// NewVerifier constructs an empty verifier. Call AddKey for each
// trusted publisher key.
func NewVerifier() *Verifier {
	return &Verifier{keys: make(map[string]ed25519.PublicKey)}
}

// AddKey registers a trusted publisher public key under an
// operator-chosen name. Name is surfaced in logs + errors so
// operators can trace "which key signed this?" during audit.
// Duplicate names overwrite — treat the map as a config reload
// target, not an append list.
func (v *Verifier) AddKey(name string, pub ed25519.PublicKey) error {
	if name == "" {
		return errors.New("skills: signing key name is required")
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("skills: signing key %q: wrong size (%d, want %d)",
			name, len(pub), ed25519.PublicKeySize)
	}
	v.keys[name] = pub
	return nil
}

// Count returns the number of registered keys — a zero-sized
// verifier with SigningRequire is operator-meaningful and callers
// may want to log the state.
func (v *Verifier) Count() int { return len(v.keys) }

// Verify returns (signer-name, true) when sig is a valid ed25519
// signature over data by any of the registered keys. A miss returns
// ("", false) — no error; missing signature isn't distinguished
// from invalid-signature at this layer because the caller's policy
// decides what either means.
func (v *Verifier) Verify(data, sig []byte) (string, bool) {
	if len(sig) != ed25519.SignatureSize {
		return "", false
	}
	for name, key := range v.keys {
		if ed25519.Verify(key, data, sig) {
			return name, true
		}
	}
	return "", false
}

// LoadTrustedPublishersFile reads a simple text file of
// "name base64-ed25519-pubkey" lines and adds each to the verifier.
// Blank lines and lines starting with "#" are comments. The format
// is intentionally minimal — no TOML nesting, no JSON schema —
// because trust roots should be human-auditable in a single
// glance.
func (v *Verifier) LoadTrustedPublishersFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("skills: read publishers file %q: %w", path, err)
	}
	lineNo := 0
	for _, line := range strings.Split(string(raw), "\n") {
		lineNo++
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return fmt.Errorf("skills: publishers file %q line %d: expected 'name base64key'",
				path, lineNo)
		}
		name, encoded := fields[0], fields[1]
		pub, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return fmt.Errorf("skills: publishers file %q line %d: decode base64: %w",
				path, lineNo, err)
		}
		if err := v.AddKey(name, pub); err != nil {
			return err
		}
	}
	return nil
}

// readSignature reads the detached signature file next to a
// manifest. Returns (nil, nil) when the file doesn't exist so the
// caller can distinguish "no signature" (acceptable under off /
// prefer) from "broken signature file" (rejected under any policy).
func readSignature(manifestPath string) ([]byte, error) {
	sigPath := manifestPath + ".sig"
	raw, err := os.ReadFile(sigPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("skills: read %q: %w", sigPath, err)
	}
	// Tolerate base64-encoded signatures (common for text-editor-
	// friendly distribution) AND raw binary.
	if decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw))); err == nil && len(decoded) == ed25519.SignatureSize {
		return decoded, nil
	}
	return raw, nil
}

// sigPathFor returns the detached-signature path convention used
// across the repo. Exposed so CLI tooling ("lobslaw skill sign")
// and the loader share the same filename.
func sigPathFor(manifestDir string) string {
	return filepath.Join(manifestDir, "manifest.yaml.sig")
}
