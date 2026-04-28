package clawhub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/storage"
)

func TestInstallDownloadsBundledBinaries(t *testing.T) {
	t.Parallel()
	binBytes := []byte("#!/bin/sh\necho hi\n")
	binSHA := sha256Hex(binBytes)
	binSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(binBytes)
	}))
	t.Cleanup(binSrv.Close)

	manifest := "name: demo\nversion: 1.0.0\nruntime: bash\nhandler: handler.sh\n" +
		"binaries:\n" +
		"  - name: gws-cli\n" +
		"    url: " + binSrv.URL + "\n" +
		"    sha256: " + binSHA + "\n" +
		"    target: bin/gws-cli\n"
	bundle := makeBundle(t, map[string]string{
		"manifest.yaml": manifest,
		"handler.sh":    "#!/bin/bash\n",
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
	binPath := filepath.Join(mountRoot, "demo", "bin", "gws-cli")
	info, err := os.Stat(binPath)
	if err != nil {
		t.Fatalf("binary not installed: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("binary should be executable; mode = %v", info.Mode())
	}
	got, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(binBytes) {
		t.Errorf("binary content mismatch")
	}
	if res.InstallDir != filepath.Join(mountRoot, "demo") {
		t.Errorf("install dir = %q", res.InstallDir)
	}
}

func TestInstallRejectsBinarySHAMismatch(t *testing.T) {
	t.Parallel()
	binSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("real binary"))
	}))
	t.Cleanup(binSrv.Close)

	manifest := "name: demo\nversion: 1.0.0\n" +
		"binaries:\n" +
		"  - name: x\n" +
		"    url: " + binSrv.URL + "\n" +
		"    sha256: 0000000000000000000000000000000000000000000000000000000000000000\n" +
		"    target: bin/x\n"
	bundle := makeBundle(t, map[string]string{"manifest.yaml": manifest})
	sha := sha256Hex(bundle)
	inst, mgr, _ := newInstallTestStack(t, bundle, sha)

	entry, _ := inst.client.GetSkill(context.Background(), "demo", "1.0.0")
	_, err := inst.Install(context.Background(), entry, InstallTarget{MountLabel: "skill-tools"})
	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Errorf("expected binary SHA mismatch, got %v", err)
	}
	mountRoot, _ := mgr.Resolve("skill-tools")
	if _, statErr := os.Stat(filepath.Join(mountRoot, "demo")); !os.IsNotExist(statErr) {
		t.Error("install dir should not exist after binary failure (rollback)")
	}
}

func TestInstallRejectsBinaryTargetTraversal(t *testing.T) {
	t.Parallel()
	binSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	t.Cleanup(binSrv.Close)

	manifest := "name: demo\nversion: 1.0.0\n" +
		"binaries:\n" +
		"  - name: bad\n" +
		"    url: " + binSrv.URL + "\n" +
		"    sha256: " + sha256Hex([]byte("x")) + "\n" +
		"    target: ../../etc/passwd\n"
	bundle := makeBundle(t, map[string]string{"manifest.yaml": manifest})
	sha := sha256Hex(bundle)
	inst, _, _ := newInstallTestStack(t, bundle, sha)
	entry, _ := inst.client.GetSkill(context.Background(), "demo", "1.0.0")
	_, err := inst.Install(context.Background(), entry, InstallTarget{MountLabel: "skill-tools"})
	if err == nil || !strings.Contains(err.Error(), "traverse") {
		t.Errorf("expected traversal rejection, got %v", err)
	}
}

func TestInstallSkippedBinaries(t *testing.T) {
	t.Parallel()
	bundle := makeBundle(t, map[string]string{
		"manifest.yaml": "name: demo\nversion: 1.0.0\n",
	})
	sha := sha256Hex(bundle)
	inst, _, _ := newInstallTestStack(t, bundle, sha)
	entry, _ := inst.client.GetSkill(context.Background(), "demo", "1.0.0")
	if _, err := inst.Install(context.Background(), entry, InstallTarget{MountLabel: "skill-tools"}); err != nil {
		t.Errorf("install with no binaries should succeed: %v", err)
	}
}

// silence unused imports in test-only helpers when binaries-free
// installs are exercised.
var _ = storage.NewManager
