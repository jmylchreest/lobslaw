package main

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/mtls"
)

// The node certificate `lobslaw init` writes has to carry the loopback
// IPs, because the CLI's fallback dial address is 127.0.0.1
// (dialableListenAddr in live_node.go) and a certificate only verifies
// for the names it carries. Bound to mtls.LoopbackIPs rather than a
// literal, so the test and the code cannot drift apart.
func TestRunInitCertNamesLoopback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ans := initAnswers{
		Dir:              dir,
		ProviderLabel:    "openrouter",
		ProviderEndpoint: "https://openrouter.ai/api/v1/chat/completions",
		ProviderModel:    "anthropic/claude-sonnet-4",
		ProviderAPIKey:   "test-key",
		SoulName:         "assistant",
	}
	if err := runInit(ans); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	certPEM, err := os.ReadFile(filepath.Join(dir, "certs", "node-cert.pem"))
	if err != nil {
		t.Fatalf("read node cert: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("node-cert.pem did not decode as PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse node cert: %v", err)
	}

	for _, want := range mtls.LoopbackIPs() {
		found := false
		for _, got := range cert.IPAddresses {
			if got.Equal(want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("node cert IPAddresses = %v, want to include %v (mtls.LoopbackIPs)", cert.IPAddresses, want)
		}
	}
}
