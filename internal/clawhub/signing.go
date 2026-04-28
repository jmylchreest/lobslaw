package clawhub

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// SigningPolicy mirrors internal/skills.SigningPolicy. Kept as a
// separate type so clawhub doesn't take a hard dep on the skills
// package signature surface — the policy semantics are identical
// but the bytes being verified differ (manifest contents vs.
// "<name>\n<version>\n<sha256>\n" canonical envelope).
type SigningPolicy string

const (
	// SigningOff ignores the SignedBy + Signature fields entirely.
	// Suitable for self-hosted clawhub deployments where the operator
	// trusts the catalog's TLS endpoint and doesn't want a separate
	// publisher-key trust store.
	SigningOff SigningPolicy = "off"

	// SigningPrefer verifies when the catalog populated SignedBy +
	// Signature, but doesn't fail the install if signatures are
	// absent. Invalid signatures still abort — "missing" is a
	// publisher choice, "tampered" is an attack.
	SigningPrefer SigningPolicy = "prefer"

	// SigningRequire mandates a valid signature on every install.
	// Catalog entries without SignedBy / Signature are rejected;
	// invalid signatures are rejected. Use this when the catalog is
	// hostile or untrusted (a public clawhub.ai mirror, say).
	SigningRequire SigningPolicy = "require"
)

// IsValid reports whether s is one of the known policies.
func (s SigningPolicy) IsValid() bool {
	switch s {
	case SigningOff, SigningPrefer, SigningRequire:
		return true
	default:
		return false
	}
}

// BundleVerifier abstracts the ed25519 publisher trust store. The
// node wires this to internal/skills.Verifier (which is also the
// store used for direct manifest-signature checks); a fake
// implementation in tests records calls + returns canned outcomes.
type BundleVerifier interface {
	// Verify returns the signer name and true on success, ("",
	// false) when no trusted key matches sig over data.
	Verify(data, sig []byte) (signer string, ok bool)

	// Count returns the number of trusted publisher keys. Used by
	// the install pipeline to decide whether SigningRequire is
	// even attempt-able — zero keys + Require is a config error.
	Count() int
}

// canonicalBundleBytes is the deterministic byte sequence the
// publisher signs. Keeping it minimal and human-readable means a
// publisher tooling can compute it with `printf` + `openssl`. The
// trailing newline is intentional — it disambiguates trailing
// whitespace.
func canonicalBundleBytes(entry *SkillEntry) []byte {
	return []byte(entry.Name + "\n" + entry.Version + "\n" + strings.ToLower(entry.BundleSHA256) + "\n")
}

// applySigningPolicy verifies the entry's signature according to
// the configured policy. Returns a descriptive error when the
// policy is violated; otherwise returns the resolved signer name
// (empty string when SigningOff).
func applySigningPolicy(entry *SkillEntry, policy SigningPolicy, verifier BundleVerifier) (string, error) {
	if !policy.IsValid() {
		return "", fmt.Errorf("clawhub: unknown signing policy %q", policy)
	}
	if policy == SigningOff {
		return "", nil
	}
	if verifier == nil || verifier.Count() == 0 {
		if policy == SigningRequire {
			return "", errors.New("clawhub: signing policy is 'require' but no trusted publisher keys are configured")
		}
		return "", nil
	}
	hasSig := entry.SignedBy != "" && entry.Signature != ""
	if !hasSig {
		if policy == SigningRequire {
			return "", fmt.Errorf("clawhub: skill %q@%s has no SignedBy/Signature but signing policy is 'require'", entry.Name, entry.Version)
		}
		return "", nil
	}
	rawSig, err := base64.StdEncoding.DecodeString(entry.Signature)
	if err != nil {
		return "", fmt.Errorf("clawhub: signature is not valid base64: %w", err)
	}
	signer, ok := verifier.Verify(canonicalBundleBytes(entry), rawSig)
	if !ok {
		return "", fmt.Errorf("clawhub: signature verification failed for skill %q@%s (claimed signer %q)", entry.Name, entry.Version, entry.SignedBy)
	}
	if signer != entry.SignedBy {
		return "", fmt.Errorf("clawhub: signature was issued by %q but catalog entry claims %q", signer, entry.SignedBy)
	}
	return signer, nil
}
