package node_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/node"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// signNodeCert generates CA + one signed node cert into tmp; returns
// the Creds ready to hand to node.New.
func signNodeCert(t *testing.T, tmp, nodeID string) *mtls.NodeCreds {
	t.Helper()
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatalf("mkdir certs: %v", err)
	}
	caCertPath := filepath.Join(tmp, "ca.pem")
	caKeyPath := filepath.Join(tmp, "ca-key.pem")
	caCertPEM, caKeyPEM, err := mtls.GenerateCA(mtls.CAOpts{CommonName: "node-test-ca"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mtls.WriteCAFiles(caCertPath, caKeyPath, caCertPEM, caKeyPEM); err != nil {
		t.Fatal(err)
	}
	ca, caKey, err := mtls.LoadCA(caCertPath, caKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := mtls.SignNodeCert(ca, caKey, mtls.SignOpts{NodeID: nodeID})
	if err != nil {
		t.Fatal(err)
	}
	nodeCertPath := filepath.Join(tmp, nodeID+".cert.pem")
	nodeKeyPath := filepath.Join(tmp, nodeID+".key.pem")
	if err := mtls.WriteNodeFiles(nodeCertPath, nodeKeyPath, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	creds, err := mtls.LoadNodeCreds(caCertPath, nodeCertPath, nodeKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	return creds
}

// clientCreds builds TLS creds for dialing an mtls-serving node. The
// loopback address doesn't match any cert SAN, so we skip ServerName
// matching but still verify the peer against the cluster CA.
func clientCreds(t *testing.T, creds *mtls.NodeCreds) credentials.TransportCredentials {
	t.Helper()
	return credentials.NewTLS(&tls.Config{
		Certificates:       []tls.Certificate{creds.NodeCert},
		RootCAs:            creds.CAPool,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("no peer certs")
			}
			peer, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			_, err = peer.Verify(x509.VerifyOptions{Roots: creds.CAPool})
			return err
		},
	})
}

// TestNewRejectsInvalidConfig covers the config-validation error
// paths that matter for operators.
func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	creds := signNodeCert(t, t.TempDir(), "node-1")

	cases := []struct {
		name string
		cfg  node.Config
	}{
		{
			name: "missing node id",
			cfg:  node.Config{ListenAddr: "127.0.0.1:0", Creds: creds},
		},
		{
			name: "missing listen addr",
			cfg:  node.Config{NodeID: "n", Creds: creds},
		},
		{
			name: "missing creds",
			cfg:  node.Config{NodeID: "n", ListenAddr: "127.0.0.1:0"},
		},
		{
			name: "memory without storage",
			cfg: node.Config{
				NodeID:     "n",
				ListenAddr: "127.0.0.1:0",
				Creds:      creds,
				DataDir:    t.TempDir(),
				Functions:  []types.NodeFunction{types.FunctionMemory},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := node.New(tc.cfg); err == nil {
				t.Errorf("New(%+v) should have errored", tc.cfg)
			}
		})
	}
}

// TestSingleNodeStartAndPolicyRoundTrip is the Phase 2.6 headline
// exit-criteria test: a node boots with memory+policy+storage
// enabled, accepts a PolicyService.AddRule RPC over mTLS gRPC, and
// the rule is readable back after a full Shutdown → new Node → restart
// cycle (proving the raft log + state.db survived).
func TestSingleNodeStartAndPolicyRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping single-node integration in short mode")
	}

	tmp := t.TempDir()
	certDir := filepath.Join(tmp, "certs")
	dataDir := filepath.Join(tmp, "data")
	nodeID := "single-node"
	creds := signNodeCert(t, certDir, nodeID)

	memoryKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	// Boot #1: bootstrap a fresh cluster, apply one rule, shut down.
	runApply := func(cfgOverride func(*node.Config)) *lobslawv1.PolicyRule {
		cfg := node.Config{
			NodeID:     nodeID,
			Functions:  []types.NodeFunction{types.FunctionMemory, types.FunctionPolicy, types.FunctionStorage},
			ListenAddr: "127.0.0.1:0",
			DataDir:    dataDir,
			Bootstrap:  true,
			Creds:      creds,
			MemoryKey:  memoryKey,
		}
		if cfgOverride != nil {
			cfgOverride(&cfg)
		}

		n, err := node.New(cfg)
		if err != nil {
			t.Fatalf("node.New: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			close(started)
			done <- n.Start(ctx)
		}()
		<-started

		// Wait for leadership — single-node bootstrap is usually instant.
		if err := n.Raft().WaitForLeader(5 * time.Second); err != nil {
			cancel()
			<-done
			t.Fatalf("WaitForLeader: %v", err)
		}

		// AddRule via gRPC.
		conn, err := grpc.NewClient(n.ListenAddr(), grpc.WithTransportCredentials(clientCreds(t, creds)))
		if err != nil {
			cancel()
			<-done
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()

		client := lobslawv1.NewPolicyServiceClient(conn)
		rule := &lobslawv1.PolicyRule{
			Id:       "test-rule-1",
			Subject:  "user:alice",
			Action:   "memory:read",
			Resource: "*",
			Effect:   "allow",
		}
		resp, err := client.AddRule(ctx, &lobslawv1.AddRuleRequest{Rule: rule})
		if err != nil {
			cancel()
			<-done
			t.Fatalf("AddRule: %v", err)
		}
		if resp.Id != rule.Id {
			t.Errorf("AddRule returned id %q, want %q", resp.Id, rule.Id)
		}

		// Read back via Policy() — same store.
		got, err := n.Policy().GetRule(rule.Id)
		if err != nil {
			cancel()
			<-done
			t.Fatalf("GetRule immediately after AddRule: %v", err)
		}
		if got.Subject != rule.Subject {
			t.Errorf("subject = %q, want %q", got.Subject, rule.Subject)
		}

		// Shutdown.
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
		return got
	}

	rule := runApply(nil)

	// Boot #2: same data-dir, no bootstrap (raft.ErrCantBootstrap is
	// silently swallowed). The rule must be visible via GetRule.
	cfg := node.Config{
		NodeID:     nodeID,
		Functions:  []types.NodeFunction{types.FunctionMemory, types.FunctionPolicy, types.FunctionStorage},
		ListenAddr: "127.0.0.1:0",
		DataDir:    dataDir,
		Bootstrap:  true, // harmless on restart thanks to ErrCantBootstrap handling
		Creds:      creds,
		MemoryKey:  memoryKey,
	}
	n2, err := node.New(cfg)
	if err != nil {
		t.Fatalf("restart: node.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- n2.Start(ctx)
	}()
	<-started

	if err := n2.Raft().WaitForLeader(5 * time.Second); err != nil {
		t.Fatalf("WaitForLeader on restart: %v", err)
	}

	got, err := n2.Policy().GetRule(rule.Id)
	if err != nil {
		t.Fatalf("after restart: GetRule: %v", err)
	}
	if got.Subject != rule.Subject {
		t.Errorf("after restart: subject %q, want %q", got.Subject, rule.Subject)
	}

	// Confirm it's actually serialised — non-empty marshalled form.
	if raw, err := proto.Marshal(got); err != nil || len(raw) == 0 {
		t.Errorf("marshal round-trip: len=%d err=%v", len(raw), err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("second Start: %v", err)
	}
}
