package node_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/node"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// TestNodeIntegrationFullStack exercises a node boot with the full
// Phase A/C/E feature set wired: mTLS, storage mount, OAuth provider
// declared, clawhub base URL set, egress UDS configured. The
// assertion is that the wire stages all execute without error and
// the resulting node serves /healthz — a smoke test that catches
// boot-time wiring regressions across the cross-cutting features.
//
// We do NOT exercise OAuth flows, clawhub installs, or netns-isolated
// skills here — those have unit tests at their respective layers.
// This test guards against the integration cliff where each piece
// works in isolation but they conflict on shared state.
func TestNodeIntegrationFullStack(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping full-stack integration in short mode")
	}

	tmp := t.TempDir()
	nodeID := "integration-full-stack"

	// mTLS: real CA + signed node cert.
	creds := mustSignNodeCertForIntegration(t, filepath.Join(tmp, "certs"), nodeID)

	// Memory key — base64-encoded 32 random bytes.
	memKeyBytes := make([]byte, 32)
	if _, err := rand.Read(memKeyBytes); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOBSLAW_INTEGRATION_MEM_KEY", base64.StdEncoding.EncodeToString(memKeyBytes))

	// Storage mount — local-disk pointed at a temp dir.
	storageRoot := filepath.Join(tmp, "skill-tools")
	if err := os.MkdirAll(storageRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	// OAuth provider — set the client_id_ref via env.
	t.Setenv("LOBSLAW_INTEGRATION_OAUTH_CID", "test-client-id")

	// Clawhub stub — minimal HTTP server so wireClawhub doesn't fail.
	clawhubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(clawhubSrv.Close)

	// Egress UDS path — under the temp dir so cleanup is automatic.
	udsPath := filepath.Join(tmp, "egress.sock")

	mockProvider := compute.NewMockProvider(compute.MockResponse{Content: "ok"})

	memKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	_ = memKeyBytes // memKey is generated above; keep the env reservation for clarity
	cfg := node.Config{
		NodeID:     nodeID,
		Functions:  []types.NodeFunction{types.FunctionMemory, types.FunctionCompute, types.FunctionGateway, types.FunctionStorage},
		ListenAddr: "127.0.0.1:0",
		Creds:      creds,
		MemoryKey:      memKey,
		DataDir:        filepath.Join(tmp, "data"),
		Bootstrap:      true,
		SnapshotTarget: "storage:skill-tools",
		LLMProvider: mockProvider,
		Gateway: config.GatewayConfig{
			Enabled:          true,
			HTTPPort:         0,
			UnknownUserScope: "public",
		},
		Storage: config.StorageConfig{
			Enabled: true,
			Mounts: []config.StorageMountConfig{
				{Label: "skill-tools", Type: "local", Path: storageRoot, Mode: "rw"},
			},
		},
		Security: config.SecurityConfig{
			ClawhubBaseURL: clawhubSrv.URL,
			EgressUDSPath:  udsPath,
			OAuth: map[string]config.OAuthProviderConfig{
				"google": {
					ClientIDRef: "env:LOBSLAW_INTEGRATION_OAUTH_CID",
				},
			},
		},
	}

	n, err := node.New(cfg)
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Start(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Log("node.Start did not return within 5s of cancel")
		}
	})

	deadline := time.Now().Add(3 * time.Second)
	for n.Gateway() == nil || n.Gateway().Addr() == "" {
		if time.Now().After(deadline) {
			t.Fatal("gateway didn't bind within 3s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	resp, err := http.Get("http://" + n.Gateway().Addr() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", resp.StatusCode)
	}

	if _, err := os.Stat(udsPath); err != nil {
		t.Errorf("egress UDS not created at %q: %v", udsPath, err)
	}
}

// mustSignNodeCertForIntegration is a tiny copy of node_test.go's
// signNodeCert — duplicated rather than exported because the
// internal/node package keeps test helpers private. Kept short on
// purpose; if the helper grows, fold both into a testutil package.
func mustSignNodeCertForIntegration(t *testing.T, dir, nodeID string) *mtls.NodeCreds {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	caCertPath := filepath.Join(dir, "ca.pem")
	caKeyPath := filepath.Join(dir, "ca-key.pem")
	caCertPEM, caKeyPEM, err := mtls.GenerateCA(mtls.CAOpts{CommonName: "test-ca"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mtls.WriteCAFiles(caCertPath, caKeyPath, caCertPEM, caKeyPEM); err != nil {
		t.Fatal(err)
	}
	ca, caPriv, err := mtls.LoadCA(caCertPath, caKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	nodeCertPEM, nodeKeyPEM, err := mtls.SignNodeCert(ca, caPriv, mtls.SignOpts{NodeID: nodeID})
	if err != nil {
		t.Fatal(err)
	}
	nodeCertPath := filepath.Join(dir, "node.pem")
	nodeKeyPath := filepath.Join(dir, "node-key.pem")
	if err := mtls.WriteNodeFiles(nodeCertPath, nodeKeyPath, nodeCertPEM, nodeKeyPEM); err != nil {
		t.Fatal(err)
	}
	creds, err := mtls.LoadNodeCreds(caCertPath, nodeCertPath, nodeKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(creds.NodeID, nodeID) {
		t.Fatalf("unexpected NodeID %q", creds.NodeID)
	}
	return creds
}
