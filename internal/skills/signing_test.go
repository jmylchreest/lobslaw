package skills

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func generateKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// writeSignedSkill creates a manifest + handler + detached ed25519
// signature in dir. Returns the manifest path for convenience.
func writeSignedSkill(t *testing.T, dir string, priv ed25519.PrivateKey) string {
	t.Helper()
	writeHandler(t, dir, "h.sh", "echo")
	body := []byte(`name: signed-skill
version: 1.0.0
runtime: bash
handler: h.sh
`)
	manifestPath := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(manifestPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, body)
	if err := os.WriteFile(manifestPath+".sig", sig, 0o644); err != nil {
		t.Fatal(err)
	}
	return manifestPath
}

// --- SigningPolicy parsing + validation ---------------------------------

func TestParseSigningPolicy(t *testing.T) {
	t.Parallel()
	cases := map[string]SigningPolicy{
		"off":     SigningOff,
		"OFF":     SigningOff,
		"prefer":  SigningPrefer,
		"require": SigningRequire,
		"":        SigningPrefer, // default
		"garbage": SigningPrefer, // unrecognised → safe default
	}
	for in, want := range cases {
		if got := ParseSigningPolicy(in); got != want {
			t.Errorf("ParseSigningPolicy(%q) = %q want %q", in, got, want)
		}
	}
}

func TestSigningPolicyIsValid(t *testing.T) {
	t.Parallel()
	for _, p := range []SigningPolicy{SigningOff, SigningPrefer, SigningRequire} {
		if !p.IsValid() {
			t.Errorf("%q should be valid", p)
		}
	}
	for _, p := range []SigningPolicy{"", "yes", "true", "REQUIRE_YES"} {
		if SigningPolicy(p).IsValid() {
			t.Errorf("%q should not be valid", p)
		}
	}
}

// --- Verifier -----------------------------------------------------------

func TestVerifierAddKeyRejectsWrongSize(t *testing.T) {
	t.Parallel()
	v := NewVerifier()
	if err := v.AddKey("bogus", []byte("too-short")); err == nil {
		t.Error("short key should be rejected")
	}
}

func TestVerifierAddKeyRejectsEmptyName(t *testing.T) {
	t.Parallel()
	v := NewVerifier()
	pub, _ := generateKeypair(t)
	if err := v.AddKey("", pub); err == nil {
		t.Error("empty name should be rejected")
	}
}

func TestVerifierVerifyHappyPath(t *testing.T) {
	t.Parallel()
	pub, priv := generateKeypair(t)
	v := NewVerifier()
	_ = v.AddKey("alice", pub)

	data := []byte("hello skill")
	sig := ed25519.Sign(priv, data)
	signer, ok := v.Verify(data, sig)
	if !ok {
		t.Fatal("valid signature should verify")
	}
	if signer != "alice" {
		t.Errorf("signer name: %q", signer)
	}
}

func TestVerifierVerifyMultipleKeys(t *testing.T) {
	t.Parallel()
	_, alicePriv := generateKeypair(t)
	bobPub, bobPriv := generateKeypair(t)
	carolPub, _ := generateKeypair(t)

	v := NewVerifier()
	_ = v.AddKey("bob", bobPub)
	_ = v.AddKey("carol", carolPub)

	data := []byte("from bob")
	aliceSig := ed25519.Sign(alicePriv, data) // alice not registered
	if _, ok := v.Verify(data, aliceSig); ok {
		t.Error("alice's signature should not verify against bob+carol")
	}

	bobSig := ed25519.Sign(bobPriv, data)
	signer, ok := v.Verify(data, bobSig)
	if !ok || signer != "bob" {
		t.Errorf("bob's signature should verify as bob; got %q ok=%v", signer, ok)
	}
}

func TestVerifierVerifyRejectsWrongSizeSignature(t *testing.T) {
	t.Parallel()
	pub, _ := generateKeypair(t)
	v := NewVerifier()
	_ = v.AddKey("k", pub)
	_, ok := v.Verify([]byte("x"), []byte("not-64-bytes"))
	if ok {
		t.Error("wrong-sized sig should not verify")
	}
}

func TestLoadTrustedPublishersFile(t *testing.T) {
	t.Parallel()
	pub, _ := generateKeypair(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "publishers")
	content := strings.Join([]string{
		"# one key per line",
		"",
		"alice " + base64.StdEncoding.EncodeToString(pub),
	}, "\n")
	_ = os.WriteFile(path, []byte(content), 0o644)

	v := NewVerifier()
	if err := v.LoadTrustedPublishersFile(path); err != nil {
		t.Fatal(err)
	}
	if v.Count() != 1 {
		t.Errorf("expected 1 key loaded; got %d", v.Count())
	}
}

func TestLoadTrustedPublishersFileRejectsMalformed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "pub")
	_ = os.WriteFile(path, []byte("alice no-space-key-on-one-token"), 0o644)
	v := NewVerifier()
	if err := v.LoadTrustedPublishersFile(path); err == nil {
		t.Error("malformed line should error")
	}
}

// --- ParseWithPolicy behaviour ------------------------------------------

func TestParseWithPolicyOffIgnoresSignature(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, priv := generateKeypair(t)
	_ = writeSignedSkill(t, dir, priv)

	// Verifier is nil under SigningOff — allowed.
	s, err := ParseWithPolicy(dir, SigningOff, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.IsSigned {
		t.Error("SigningOff must report IsSigned=false regardless of signature presence")
	}
}

func TestParseWithPolicyPreferAcceptsUnsigned(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeHandler(t, dir, "h.sh", "echo")
	writeManifest(t, dir, `
name: plain
version: 1.0.0
runtime: bash
handler: h.sh
`)
	s, err := ParseWithPolicy(dir, SigningPrefer, NewVerifier())
	if err != nil {
		t.Fatal(err)
	}
	if s.IsSigned {
		t.Error("unsigned manifest should have IsSigned=false")
	}
}

func TestParseWithPolicyPreferAcceptsValidSignature(t *testing.T) {
	t.Parallel()
	pub, priv := generateKeypair(t)
	dir := t.TempDir()
	_ = writeSignedSkill(t, dir, priv)

	v := NewVerifier()
	_ = v.AddKey("publisher-1", pub)

	s, err := ParseWithPolicy(dir, SigningPrefer, v)
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsSigned {
		t.Error("valid signature should set IsSigned=true")
	}
	if s.SignedBy != "publisher-1" {
		t.Errorf("SignedBy: %q", s.SignedBy)
	}
}

func TestParseWithPolicyPreferRejectsInvalidSignature(t *testing.T) {
	t.Parallel()
	// Publisher key WRONG — sig is from priv but verifier doesn't
	// know that key.
	_, priv := generateKeypair(t)
	otherPub, _ := generateKeypair(t)

	dir := t.TempDir()
	_ = writeSignedSkill(t, dir, priv)

	v := NewVerifier()
	_ = v.AddKey("wrong-one", otherPub)

	_, err := ParseWithPolicy(dir, SigningPrefer, v)
	if err == nil {
		t.Fatal("sig present but not-verifying must reject even under Prefer")
	}
	if !strings.Contains(err.Error(), "did not verify") {
		t.Errorf("error should mention verification; got %v", err)
	}
}

func TestParseWithPolicyRequireRejectsUnsigned(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeHandler(t, dir, "h.sh", "echo")
	writeManifest(t, dir, `
name: plain
version: 1.0.0
runtime: bash
handler: h.sh
`)
	_, err := ParseWithPolicy(dir, SigningRequire, NewVerifier())
	if err == nil {
		t.Fatal("SigningRequire must reject unsigned manifest")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error should mention 'required'; got %v", err)
	}
}

func TestParseWithPolicyRequireAcceptsValidSignature(t *testing.T) {
	t.Parallel()
	pub, priv := generateKeypair(t)
	dir := t.TempDir()
	_ = writeSignedSkill(t, dir, priv)

	v := NewVerifier()
	_ = v.AddKey("p", pub)

	s, err := ParseWithPolicy(dir, SigningRequire, v)
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsSigned {
		t.Error("Require-path valid-sig should set IsSigned=true")
	}
}

func TestParseWithPolicyRequireWithNilVerifierErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeHandler(t, dir, "h.sh", "echo")
	writeManifest(t, dir, `
name: plain
version: 1.0.0
runtime: bash
handler: h.sh
`)
	_, err := ParseWithPolicy(dir, SigningRequire, nil)
	if err == nil {
		t.Error("SigningRequire with nil verifier should error at construction-time")
	}
}

// --- Registry preference -------------------------------------------------

func TestRegistryPreferSignedBreaksTies(t *testing.T) {
	t.Parallel()
	r := NewRegistryWithPolicy(nil, SigningPrefer)

	unsigned := &Skill{
		Manifest:    Manifest{Name: "s", Version: "1.0.0"},
		ManifestDir: "/mnt/a",
	}
	signed := &Skill{
		Manifest:    Manifest{Name: "s", Version: "1.0.0"},
		ManifestDir: "/mnt/z", // lexicographically LATER
		IsSigned:    true,
		SignedBy:    "publisher-1",
	}

	r.Put(unsigned)
	r.Put(signed)

	got, _ := r.Get("s")
	if !got.IsSigned {
		t.Errorf("PreferSigned should pick the signed candidate; got %+v", got)
	}
}

func TestRegistryNoPreferFallsBackToLexicographic(t *testing.T) {
	t.Parallel()
	r := NewRegistryWithPolicy(nil, SigningOff)

	a := &Skill{Manifest: Manifest{Name: "s", Version: "1.0.0"}, ManifestDir: "/mnt/a"}
	z := &Skill{Manifest: Manifest{Name: "s", Version: "1.0.0"}, ManifestDir: "/mnt/z", IsSigned: true}

	r.Put(a)
	r.Put(z)

	got, _ := r.Get("s")
	if got.ManifestDir != "/mnt/a" {
		t.Errorf("SigningOff should stick to lexicographic tiebreak; got %q", got.ManifestDir)
	}
}

// TestRegistryHigherSemverStillBeatsSignedOlder — even under
// preferSigned, a higher-version unsigned candidate wins. Signing
// is only a TIE-breaker, not an override.
func TestRegistryHigherSemverStillBeatsSignedOlder(t *testing.T) {
	t.Parallel()
	r := NewRegistryWithPolicy(nil, SigningPrefer)
	signedOld := &Skill{
		Manifest: Manifest{Name: "s", Version: "1.0.0"},
		ManifestDir: "/mnt/a", IsSigned: true,
	}
	unsignedNew := &Skill{
		Manifest: Manifest{Name: "s", Version: "2.0.0"},
		ManifestDir: "/mnt/b",
	}
	r.Put(signedOld)
	r.Put(unsignedNew)

	got, _ := r.Get("s")
	if got.Manifest.Version != "2.0.0" {
		t.Errorf("higher semver should win even if older is signed; got %+v", got)
	}
}

// --- Compile-time check -------------------------------------------------

var _ = errors.New // reserve in case future edits trim other imports
