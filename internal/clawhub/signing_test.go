package clawhub

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/storage"
)

type fakeVerifier struct {
	keys map[string]ed25519.PublicKey
}

func newFakeVerifier(t *testing.T, name string) (*fakeVerifier, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeVerifier{keys: map[string]ed25519.PublicKey{name: pub}}, priv
}

func (f *fakeVerifier) Verify(data, sig []byte) (string, bool) {
	for name, pub := range f.keys {
		if ed25519.Verify(pub, data, sig) {
			return name, true
		}
	}
	return "", false
}

func (f *fakeVerifier) Count() int { return len(f.keys) }

func signEntry(t *testing.T, priv ed25519.PrivateKey, entry *SkillEntry) {
	t.Helper()
	sig := ed25519.Sign(priv, canonicalBundleBytes(entry))
	entry.Signature = base64.StdEncoding.EncodeToString(sig)
}

func TestApplySigningPolicyOffSkipsEverything(t *testing.T) {
	t.Parallel()
	entry := &SkillEntry{Name: "x", Version: "1.0.0", BundleSHA256: "abc"}
	signer, err := applySigningPolicy(entry, SigningOff, nil)
	if err != nil || signer != "" {
		t.Errorf("Off should pass with empty signer; got %q, %v", signer, err)
	}
}

func TestApplySigningPolicyPreferAcceptsMissingSig(t *testing.T) {
	t.Parallel()
	entry := &SkillEntry{Name: "x", Version: "1.0.0", BundleSHA256: "abc"}
	v, _ := newFakeVerifier(t, "alice")
	signer, err := applySigningPolicy(entry, SigningPrefer, v)
	if err != nil {
		t.Errorf("Prefer should accept missing signature; got %v", err)
	}
	if signer != "" {
		t.Errorf("signer should be empty when entry is unsigned; got %q", signer)
	}
}

func TestApplySigningPolicyPreferRejectsBadSig(t *testing.T) {
	t.Parallel()
	v, _ := newFakeVerifier(t, "alice")
	entry := &SkillEntry{
		Name: "x", Version: "1.0.0", BundleSHA256: "abc",
		SignedBy:  "alice",
		Signature: base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
	if _, err := applySigningPolicy(entry, SigningPrefer, v); err == nil {
		t.Error("Prefer must still reject a bad signature")
	}
}

func TestApplySigningPolicyRequireAcceptsValidSig(t *testing.T) {
	t.Parallel()
	v, priv := newFakeVerifier(t, "alice")
	entry := &SkillEntry{
		Name: "x", Version: "1.0.0", BundleSHA256: "abc", SignedBy: "alice",
	}
	signEntry(t, priv, entry)
	signer, err := applySigningPolicy(entry, SigningRequire, v)
	if err != nil {
		t.Fatalf("Require with valid sig: %v", err)
	}
	if signer != "alice" {
		t.Errorf("signer = %q", signer)
	}
}

func TestApplySigningPolicyRequireRejectsMissing(t *testing.T) {
	t.Parallel()
	v, _ := newFakeVerifier(t, "alice")
	entry := &SkillEntry{Name: "x", Version: "1.0.0", BundleSHA256: "abc"}
	if _, err := applySigningPolicy(entry, SigningRequire, v); err == nil {
		t.Error("Require should reject unsigned entries")
	}
}

func TestApplySigningPolicyRejectsSignerMismatch(t *testing.T) {
	t.Parallel()
	v, priv := newFakeVerifier(t, "alice")
	entry := &SkillEntry{
		Name: "x", Version: "1.0.0", BundleSHA256: "abc", SignedBy: "bob",
	}
	signEntry(t, priv, entry)
	if _, err := applySigningPolicy(entry, SigningRequire, v); err == nil || !strings.Contains(err.Error(), "claims") {
		t.Errorf("expected signer-mismatch rejection; got %v", err)
	}
}

func TestApplySigningPolicyRejectsBadBase64(t *testing.T) {
	t.Parallel()
	v, _ := newFakeVerifier(t, "alice")
	entry := &SkillEntry{
		Name: "x", Version: "1.0.0", BundleSHA256: "abc",
		SignedBy: "alice", Signature: "!!!not-base64!!!",
	}
	if _, err := applySigningPolicy(entry, SigningRequire, v); err == nil {
		t.Error("non-base64 signature should be rejected")
	}
}

func TestInstallEnforcesSigningRequire(t *testing.T) {
	t.Parallel()
	bundle := makeBundle(t, map[string]string{
		"manifest.yaml": "name: demo\nversion: 1.0.0\n",
	})
	sha := sha256Hex(bundle)
	v, priv := newFakeVerifier(t, "alice")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bundle)
	}))
	t.Cleanup(srv.Close)

	entry := &SkillEntry{
		Name: "demo", Version: "1.0.0", BundleSHA256: sha,
		SignedBy:  "alice",
		BundleURL: srv.URL,
	}
	signEntry(t, priv, entry)

	mgr := storage.NewManager()
	mountRoot := t.TempDir()
	_ = mgr.Register(context.Background(), &fakeMount{label: "skill-tools", path: mountRoot})

	c, _ := NewClient("https://x.invalid")
	inst, err := NewInstaller(InstallerConfig{
		Client: c, Storage: mgr,
		Policy: SigningRequire, Verifier: v,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := inst.Install(context.Background(), entry, InstallTarget{MountLabel: "skill-tools"})
	if err != nil {
		t.Fatalf("Install with valid sig: %v", err)
	}
	if res.SignedBy != "alice" {
		t.Errorf("result SignedBy = %q", res.SignedBy)
	}
}

func TestInstallSigningRequireRejectsBadSig(t *testing.T) {
	t.Parallel()
	bundle := makeBundle(t, map[string]string{"manifest.yaml": "name: x\n"})
	sha := sha256Hex(bundle)
	v, _ := newFakeVerifier(t, "alice")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bundle)
	}))
	t.Cleanup(srv.Close)
	entry := &SkillEntry{
		Name: "demo", Version: "1.0.0", BundleSHA256: sha,
		BundleURL: srv.URL,
		SignedBy:  "alice",
		Signature: base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
	mgr := storage.NewManager()
	_ = mgr.Register(context.Background(), &fakeMount{label: "skill-tools", path: t.TempDir()})
	c, _ := NewClient("https://x.invalid")
	inst, _ := NewInstaller(InstallerConfig{
		Client: c, Storage: mgr,
		Policy: SigningRequire, Verifier: v,
	})
	if _, err := inst.Install(context.Background(), entry, InstallTarget{MountLabel: "skill-tools"}); err == nil {
		t.Error("Install with bad signature must fail under SigningRequire")
	}
}
