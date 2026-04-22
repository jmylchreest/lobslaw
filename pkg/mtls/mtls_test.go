package mtls

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Helper: create a fresh CA + sign one node cert. Returns the file
// paths the rest of the tests work against.
func setupCluster(t *testing.T, nodeID string) (caCert, caKey, nodeCert, nodeKey string) {
	t.Helper()
	dir := t.TempDir()
	caCert = filepath.Join(dir, "ca.pem")
	caKey = filepath.Join(dir, "ca-key.pem")
	nodeCert = filepath.Join(dir, "node-cert.pem")
	nodeKey = filepath.Join(dir, "node-key.pem")

	caCertPEM, caKeyPEM, err := GenerateCA(CAOpts{CommonName: "test-ca"})
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	if err := WriteCAFiles(caCert, caKey, caCertPEM, caKeyPEM); err != nil {
		t.Fatalf("WriteCAFiles: %v", err)
	}

	ca, caPriv, err := LoadCA(caCert, caKey)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	certPEM, keyPEM, err := SignNodeCert(ca, caPriv, SignOpts{NodeID: nodeID})
	if err != nil {
		t.Fatalf("SignNodeCert: %v", err)
	}
	if err := WriteNodeFiles(nodeCert, nodeKey, certPEM, keyPEM); err != nil {
		t.Fatalf("WriteNodeFiles: %v", err)
	}
	return
}

func TestRoundTripCAAndNodeCert(t *testing.T) {
	t.Parallel()
	caCert, _, nodeCert, nodeKey := setupCluster(t, "node-1")

	creds, err := LoadNodeCreds(caCert, nodeCert, nodeKey)
	if err != nil {
		t.Fatalf("LoadNodeCreds: %v", err)
	}
	if creds.NodeID != "node-1" {
		t.Errorf("NodeID = %q, want node-1", creds.NodeID)
	}
	if len(creds.NodeCert.Certificate) != 1 {
		t.Errorf("cert chain length = %d, want 1", len(creds.NodeCert.Certificate))
	}
	if creds.ServerCreds() == nil {
		t.Error("ServerCreds returned nil")
	}
	if creds.ClientCreds() == nil {
		t.Error("ClientCreds returned nil")
	}
}

func TestWriteCAFilesRefusesOverwrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	caCert := filepath.Join(dir, "ca.pem")
	caKey := filepath.Join(dir, "ca-key.pem")

	certPEM, keyPEM, err := GenerateCA(CAOpts{CommonName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCAFiles(caCert, caKey, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	if err := WriteCAFiles(caCert, caKey, certPEM, keyPEM); err == nil {
		t.Error("WriteCAFiles should refuse to overwrite existing files")
	} else if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadNodeCredsMissingNodeCert(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	caCert := filepath.Join(dir, "ca.pem")
	caKey := filepath.Join(dir, "ca-key.pem")

	certPEM, keyPEM, err := GenerateCA(CAOpts{CommonName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCAFiles(caCert, caKey, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}

	_, err = LoadNodeCreds(caCert, filepath.Join(dir, "missing.pem"), filepath.Join(dir, "missing-key.pem"))
	if err == nil {
		t.Fatal("expected error for missing node cert")
	}
	if !strings.Contains(err.Error(), "cluster sign-node") {
		t.Errorf("error should direct to sign-node: %v", err)
	}
}

func TestLoadNodeCredsRejectsForeignCA(t *testing.T) {
	t.Parallel()
	_, _, nodeCert, nodeKey := setupCluster(t, "node-1")

	// Generate a second, unrelated CA and try to verify node-1 against it.
	dir := t.TempDir()
	foreignCA := filepath.Join(dir, "foreign-ca.pem")
	foreignKey := filepath.Join(dir, "foreign-key.pem")
	certPEM, keyPEM, err := GenerateCA(CAOpts{CommonName: "foreign"})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCAFiles(foreignCA, foreignKey, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}

	_, err = LoadNodeCreds(foreignCA, nodeCert, nodeKey)
	if err == nil {
		t.Fatal("expected error when verifying node-1 against foreign CA")
	}
	if !strings.Contains(err.Error(), "not signed by cluster CA") {
		t.Errorf("error should indicate CA mismatch: %v", err)
	}
}

func TestSignNodeCertRequiresNodeID(t *testing.T) {
	t.Parallel()
	caCertPEM, caKeyPEM, err := GenerateCA(CAOpts{CommonName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")
	if err := WriteCAFiles(caPath, keyPath, caCertPEM, caKeyPEM); err != nil {
		t.Fatal(err)
	}
	ca, key, err := LoadCA(caPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = SignNodeCert(ca, key, SignOpts{})
	if err == nil {
		t.Error("SignNodeCert with empty NodeID should error")
	}
}

func TestCAValidityDefaults(t *testing.T) {
	t.Parallel()
	certPEM, _, err := GenerateCA(CAOpts{CommonName: "test", Now: time.Unix(1_700_000_000, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(certPEM), "BEGIN CERTIFICATE") {
		t.Error("CA cert PEM malformed")
	}
}

func TestKeyFilesWrittenWithStrictPerms(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	caCert := filepath.Join(dir, "ca.pem")
	caKey := filepath.Join(dir, "ca-key.pem")

	certPEM, keyPEM, err := GenerateCA(CAOpts{CommonName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCAFiles(caCert, caKey, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(caKey)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("CA key perm = %o, want 600", perm)
	}
}
