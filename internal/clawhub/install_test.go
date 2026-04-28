package clawhub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/binaries"
	"github.com/jmylchreest/lobslaw/internal/storage"
)

func newTestSatisfier(t *testing.T, prefix string) *binaries.Satisfier {
	t.Helper()
	return binaries.New(binaries.Config{
		InstallPrefix: prefix,
	})
}

func makeBundle(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type fakeMount struct {
	label string
	path  string
}

func (f *fakeMount) Label() string                 { return f.label }
func (f *fakeMount) Backend() string               { return "fake" }
func (f *fakeMount) Path() string                  { return f.path }
func (f *fakeMount) Start(_ context.Context) error { return nil }
func (f *fakeMount) Stop(_ context.Context) error  { return nil }
func (f *fakeMount) Healthy() bool                 { return true }

func newInstallTestStack(t *testing.T, bundle []byte, sha string) (*Installer, *storage.Manager, string) {
	t.Helper()

	mountRoot := t.TempDir()
	mgr := storage.NewManager()
	if err := mgr.Register(context.Background(), &fakeMount{
		label: "skill-tools", path: mountRoot,
	}); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/skills/", func(w http.ResponseWriter, r *http.Request) {
		entry := SkillEntry{
			Name:         "demo",
			Version:      "1.0.0",
			BundleURL:    "http://" + r.Host + "/bundle.tgz",
			BundleSHA256: sha,
		}
		_ = json.NewEncoder(w).Encode(entry)
	})
	mux.HandleFunc("/bundle.tgz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bundle)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := NewInstaller(InstallerConfig{Client: c, Storage: mgr})
	if err != nil {
		t.Fatal(err)
	}
	return inst, mgr, srv.URL
}

// makeZipBundleFromFiles builds a clawhub-format ZIP for the slug
// install test. Mirrors the actual clawhub.ai download payload
// shape: SKILL.md (with clawdbot front-matter) + _meta.json.
func makeZipBundleFromFiles(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	return makeZipBundle(t, files)
}

func TestInstallBySlugClawhubFormat(t *testing.T) {
	t.Parallel()
	skillMD := `---
name: gog
description: Google Workspace CLI
metadata: {"clawdbot":{"requires":{"bins":["gog"]},"install":[{"id":"brew","kind":"brew","formula":"steipete/tap/gogcli","bins":["gog"]}]}}
---

# gog

Use gog for Gmail/Calendar.
`
	bundle := makeZipBundleFromFiles(t, map[string][]byte{
		"SKILL.md":   []byte(skillMD),
		"_meta.json": []byte(`{"slug":"gog","version":"1.0.0"}`),
	})

	mountRoot := t.TempDir()
	mgr := storage.NewManager()
	if err := mgr.Register(context.Background(), &fakeMount{
		label: "skill-tools", path: mountRoot,
	}); err != nil {
		t.Fatal(err)
	}

	prefix := t.TempDir()
	binDir := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "gog"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/download", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("slug"); got != "gog" {
			t.Errorf("server saw slug=%q (expected bare 'gog' after owner strip)", got)
		}
		_, _ = w.Write(bundle)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	satisfier := newTestSatisfier(t, prefix)
	inst, err := NewInstaller(InstallerConfig{
		Client:    c,
		Storage:   mgr,
		Satisfier: satisfier,
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := inst.InstallBySlug(context.Background(), "steipete/gog", InstallTarget{
		MountLabel: "skill-tools",
	})
	if err != nil {
		t.Fatalf("InstallBySlug: %v", err)
	}
	if res.Name != "gog" {
		t.Errorf("name: %q", res.Name)
	}
	wantDir := filepath.Join(mountRoot, "gog")
	if res.InstallDir != wantDir {
		t.Errorf("install dir: %q want %q", res.InstallDir, wantDir)
	}
	manifestPath := filepath.Join(wantDir, "manifest.yaml")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Errorf("synthetic manifest.yaml not present: %v", err)
	}
	skillMDPath := filepath.Join(wantDir, "SKILL.md")
	if _, err := os.Stat(skillMDPath); err != nil {
		t.Errorf("SKILL.md not preserved at install dir: %v", err)
	}
}

func TestInstallBySlugRejectsEmpty(t *testing.T) {
	t.Parallel()
	c, _ := NewClient("http://localhost:65535")
	inst, _ := NewInstaller(InstallerConfig{
		Client:  c,
		Storage: storage.NewManager(),
	})
	_, err := inst.InstallBySlug(context.Background(), "", InstallTarget{MountLabel: "skill-tools"})
	if err == nil || !strings.Contains(err.Error(), "slug required") {
		t.Errorf("expected slug-required error, got: %v", err)
	}
}

func TestInstallBySlugStripsOwnerPrefix(t *testing.T) {
	t.Parallel()
	skillMD := `---
name: gog
metadata: {"clawdbot":{"requires":{"bins":[]},"install":[]}}
---

# gog
`
	bundle := makeZipBundleFromFiles(t, map[string][]byte{
		"SKILL.md": []byte(skillMD),
	})
	mountRoot := t.TempDir()
	mgr := storage.NewManager()
	if err := mgr.Register(context.Background(), &fakeMount{
		label: "skill-tools", path: mountRoot,
	}); err != nil {
		t.Fatal(err)
	}

	var seenSlug string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/download", func(w http.ResponseWriter, r *http.Request) {
		seenSlug = r.URL.Query().Get("slug")
		_, _ = w.Write(bundle)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, _ := NewClient(srv.URL)
	inst, _ := NewInstaller(InstallerConfig{Client: c, Storage: mgr})

	if _, err := inst.InstallBySlug(context.Background(), "steipete/gog", InstallTarget{
		MountLabel: "skill-tools",
	}); err != nil {
		t.Fatalf("InstallBySlug: %v", err)
	}
	if seenSlug != "gog" {
		t.Errorf("server saw slug=%q, expected owner prefix stripped to bare 'gog'", seenSlug)
	}
}

func TestInstallExtractsBundleIntoMountSubpath(t *testing.T) {
	t.Parallel()
	bundle := makeBundle(t, map[string]string{
		"manifest.yaml":    "name: demo\nversion: 1.0.0\nruntime: bash\nhandler: handler.sh\n",
		"handler.sh":       "#!/bin/bash\necho hi\n",
		"sub/data.txt":     "payload",
	})
	sha := sha256Hex(bundle)
	inst, mgr, _ := newInstallTestStack(t, bundle, sha)

	entry, err := inst.client.GetSkill(context.Background(), "demo", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	res, err := inst.Install(context.Background(), entry, InstallTarget{
		MountLabel: "skill-tools",
		Subpath:    "demo",
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	mountRoot, _ := mgr.Resolve("skill-tools")
	wantDir := filepath.Join(mountRoot, "demo")
	if res.InstallDir != wantDir {
		t.Errorf("install dir = %q, want %q", res.InstallDir, wantDir)
	}
	for _, name := range []string{"manifest.yaml", "handler.sh", "sub/data.txt"} {
		if _, err := os.Stat(filepath.Join(wantDir, name)); err != nil {
			t.Errorf("expected %s installed: %v", name, err)
		}
	}
}

func TestInstallRejectsSHAMismatch(t *testing.T) {
	t.Parallel()
	bundle := makeBundle(t, map[string]string{
		"manifest.yaml": "name: demo\nversion: 1.0.0\n",
	})
	inst, _, _ := newInstallTestStack(t, bundle, "deadbeef")
	entry, _ := inst.client.GetSkill(context.Background(), "demo", "1.0.0")
	_, err := inst.Install(context.Background(), entry, InstallTarget{MountLabel: "skill-tools"})
	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Errorf("expected SHA-256 mismatch, got %v", err)
	}
}

func TestInstallRequiresManifestYAML(t *testing.T) {
	t.Parallel()
	bundle := makeBundle(t, map[string]string{
		"handler.sh": "#!/bin/bash\necho\n",
	})
	sha := sha256Hex(bundle)
	inst, _, _ := newInstallTestStack(t, bundle, sha)
	entry, _ := inst.client.GetSkill(context.Background(), "demo", "1.0.0")
	_, err := inst.Install(context.Background(), entry, InstallTarget{MountLabel: "skill-tools"})
	if err == nil || !strings.Contains(err.Error(), "manifest.yaml") {
		t.Errorf("expected manifest.yaml requirement, got %v", err)
	}
}

func TestExtractTarGzRejectsTraversalEntry(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: "../escape.txt", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	_ = gz.Close()
	dst := t.TempDir()
	err := extractTarGz(&buf, dst)
	if err == nil || !strings.Contains(err.Error(), "traverses parent") {
		t.Errorf("expected traversal rejection, got %v", err)
	}
}

func TestExtractTarGzRejectsAbsoluteEntry(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{
		Name: "/etc/passwd", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg,
	})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	_ = gz.Close()
	err := extractTarGz(&buf, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("expected absolute-path rejection, got %v", err)
	}
}

func TestExtractTarGzRejectsSymlink(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{
		Name: "evil", Linkname: "../target", Typeflag: tar.TypeSymlink,
	})
	_ = tw.Close()
	_ = gz.Close()
	err := extractTarGz(&buf, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unsupported entry") {
		t.Errorf("expected symlink rejection, got %v", err)
	}
}

func TestNewInstallerRequiresClientAndStorage(t *testing.T) {
	t.Parallel()
	if _, err := NewInstaller(InstallerConfig{}); err == nil {
		t.Error("nil deps should fail")
	}
	c, _ := NewClient("https://x.invalid")
	if _, err := NewInstaller(InstallerConfig{Client: c}); err == nil {
		t.Error("nil storage should fail")
	}
}

func TestNewInstallerRejectsRequireWithoutKeys(t *testing.T) {
	t.Parallel()
	c, _ := NewClient("https://x.invalid")
	mgr := storage.NewManager()
	_, err := NewInstaller(InstallerConfig{
		Client:  c,
		Storage: mgr,
		Policy:  SigningRequire,
	})
	if err == nil {
		t.Error("SigningRequire with no verifier should fail at construction")
	}
}

// Trim any trailing path separator just to keep test assertions
// readable — t.TempDir() doesn't add one, but build-tag variants
// might. Defensive helper, not used in production code.
var _ = io.Discard
