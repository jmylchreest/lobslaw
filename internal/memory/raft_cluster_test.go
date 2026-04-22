package memory_test

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/rafttransport"
)

type clusterNode struct {
	id        string
	addr      string
	server    *grpc.Server
	listener  net.Listener
	transport *rafttransport.Transport
	raft      *memory.RaftNode
	store     *memory.Store
	fsm       *memory.FSM
}

func (c *clusterNode) shutdown() {
	if c.raft != nil {
		_ = c.raft.Shutdown()
	}
	if c.server != nil {
		c.server.Stop()
	}
	if c.listener != nil {
		_ = c.listener.Close()
	}
	if c.store != nil {
		_ = c.store.Close()
	}
}

// TestRaftClusterOverGRPC stands up a 3-node Raft cluster where each
// node listens on its own localhost gRPC port, peers mutually TLS-
// authenticate against a shared cluster CA, and the transport is the
// pkg/rafttransport wrapper over Jille/raft-grpc-transport. It proves
// end-to-end that a log entry applied on the leader replicates to both
// followers and is readable from their bbolt state stores.
func TestRaftClusterOverGRPC(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-node integration in short mode")
	}

	certDir := t.TempDir()
	caCertPath := filepath.Join(certDir, "ca.pem")
	caKeyPath := filepath.Join(certDir, "ca-key.pem")

	// Cluster-wide CA.
	caCertPEM, caKeyPEM, err := mtls.GenerateCA(mtls.CAOpts{CommonName: "cluster-test-ca"})
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	if err := mtls.WriteCAFiles(caCertPath, caKeyPath, caCertPEM, caKeyPEM); err != nil {
		t.Fatal(err)
	}
	caCert, caKey, err := mtls.LoadCA(caCertPath, caKeyPath)
	if err != nil {
		t.Fatal(err)
	}

	// Build three nodes. Each reserves its port up front so peers can
	// dial the static address we record in raft configuration.
	const nodeCount = 3
	nodes := make([]*clusterNode, nodeCount)
	for i := 0; i < nodeCount; i++ {
		nodes[i] = newClusterNode(t, i, certDir, caCertPath, caCert, caKey)
	}
	defer func() {
		for _, n := range nodes {
			n.shutdown()
		}
	}()

	// Create the rafttransport for each node and register its service
	// on the gRPC server BEFORE calling Serve — gRPC refuses
	// RegisterService after Serve.
	for i, n := range nodes {
		rt, err := rafttransport.New(rafttransport.Config{
			LocalAddr: raft.ServerAddress(n.addr),
			DialOpts:  []grpc.DialOption{grpc.WithTransportCredentials(clientCredsFor(t, certDir, i))},
		})
		if err != nil {
			t.Fatalf("rafttransport.New for %s: %v", n.id, err)
		}
		rt.Register(n.server)
		n.transport = rt
	}

	// Now it's safe to start serving.
	for _, n := range nodes {
		go func(server *grpc.Server, ln net.Listener) {
			_ = server.Serve(ln)
		}(n.server, n.listener)
	}

	// Construct each RaftNode now that peer servers are listening.
	for _, n := range nodes {
		r, err := memory.NewRaft(memory.RaftConfig{
			NodeID:    n.id,
			LocalAddr: raft.ServerAddress(n.addr),
			DataDir:   filepath.Join(t.TempDir(), n.id),
			Bootstrap: false,
			Transport: n.transport.RaftTransport(),
		}, n.fsm)
		if err != nil {
			t.Fatalf("NewRaft %s: %v", n.id, err)
		}
		n.raft = r
	}

	// Bootstrap node 0 as the single initial voter, wait for leadership.
	leader := nodes[0]
	bootstrapFuture := leader.raft.Raft.BootstrapCluster(raft.Configuration{
		Servers: []raft.Server{{
			ID:       raft.ServerID(leader.id),
			Address:  raft.ServerAddress(leader.addr),
			Suffrage: raft.Voter,
		}},
	})
	if err := bootstrapFuture.Error(); err != nil && err != raft.ErrCantBootstrap {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := leader.raft.WaitForLeader(5 * time.Second); err != nil {
		t.Fatalf("leader election: %v", err)
	}

	// Join the other two as voting members.
	for i := 1; i < nodeCount; i++ {
		if err := leader.raft.AddVoter(
			raft.ServerID(nodes[i].id),
			raft.ServerAddress(nodes[i].addr),
		); err != nil {
			t.Fatalf("AddVoter %s: %v", nodes[i].id, err)
		}
	}

	// Apply a PolicyRule on the leader.
	entry := &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT,
		Id: "replicated-rule",
		Payload: &lobslawv1.LogEntry_PolicyRule{
			PolicyRule: &lobslawv1.PolicyRule{
				Id:       "replicated-rule",
				Subject:  "user:alice",
				Action:   "memory:read",
				Resource: "*",
				Effect:   "allow",
			},
		},
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leader.raft.Apply(data, 5*time.Second); err != nil {
		t.Fatalf("Apply on leader: %v", err)
	}

	// Poll each node's store for the replicated record.
	for _, n := range nodes {
		waitForRecord(t, n.fsm, "replicated-rule", 5*time.Second)
		raw, err := n.fsm.Store().Get(memory.BucketPolicyRules, "replicated-rule")
		if err != nil {
			t.Fatalf("%s: Store.Get: %v", n.id, err)
		}
		var got lobslawv1.PolicyRule
		if err := proto.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s: unmarshal: %v", n.id, err)
		}
		if got.Subject != "user:alice" {
			t.Errorf("%s: subject %q, want user:alice", n.id, got.Subject)
		}
	}
}

// newClusterNode sets up a node's certs, listener, gRPC server, store
// and FSM. The raft.Transport + RaftNode are assembled by the caller
// after all servers are running so peer dials succeed.
func newClusterNode(t *testing.T, index int, certDir, caCertPath string, caCert *x509.Certificate, caKey ed25519.PrivateKey) *clusterNode {
	t.Helper()

	id := fmt.Sprintf("node-%d", index)

	// Pre-bind a port so the address is stable in raft config.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for %s: %v", id, err)
	}
	addr := ln.Addr().String()

	// Sign a per-node cert. Include the loopback IP in SANs so TLS
	// validation against 127.0.0.1:port works.
	certPEM, keyPEM, err := mtls.SignNodeCert(caCert, caKey, mtls.SignOpts{
		NodeID: id,
	})
	if err != nil {
		t.Fatalf("SignNodeCert %s: %v", id, err)
	}
	nodeCertPath := filepath.Join(certDir, id+".cert.pem")
	nodeKeyPath := filepath.Join(certDir, id+".key.pem")
	if err := mtls.WriteNodeFiles(nodeCertPath, nodeKeyPath, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}

	creds, err := mtls.LoadNodeCreds(caCertPath, nodeCertPath, nodeKeyPath)
	if err != nil {
		t.Fatalf("LoadNodeCreds %s: %v", id, err)
	}
	server := grpc.NewServer(grpc.Creds(creds.ServerCreds()))

	// State store + FSM.
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(t.TempDir(), id+"-state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	fsm := memory.NewFSM(store)

	return &clusterNode{
		id:       id,
		addr:     addr,
		server:   server,
		listener: ln,
		store:    store,
		fsm:      fsm,
	}
}

// clientCredsFor loads node i's cert/key and returns client-side
// TransportCredentials. The mTLS handshake uses the SAN "node-N"
// embedded in the cert, but we dial the peer at 127.0.0.1:port,
// so we override ServerName via a custom tls.Config.
func clientCredsFor(t *testing.T, certDir string, index int) credentials.TransportCredentials {
	t.Helper()
	id := fmt.Sprintf("node-%d", index)
	nodeCertPath := filepath.Join(certDir, id+".cert.pem")
	nodeKeyPath := filepath.Join(certDir, id+".key.pem")
	caCertPath := filepath.Join(certDir, "ca.pem")

	creds, err := mtls.LoadNodeCreds(caCertPath, nodeCertPath, nodeKeyPath)
	if err != nil {
		t.Fatalf("LoadNodeCreds for client %s: %v", id, err)
	}
	// Build a TLS config that doesn't require the ServerName to match
	// the certificate CN/SAN — each peer dials a 127.0.0.1:port address
	// which isn't in any cert's SAN. Skipping ServerName verification
	// is safe because we still enforce CA-based cert verification
	// (RootCAs is set via creds.ClientCreds()), so only cluster-signed
	// certs are accepted.
	return credentials.NewTLS(&tls.Config{
		Certificates:       []tls.Certificate{creds.NodeCert},
		RootCAs:            creds.CAPool,
		InsecureSkipVerify: true, // server identity still validated via VerifyPeerCertificate
		MinVersion:         tls.VersionTLS13,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("peer presented no certs")
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

// waitForRecord polls fsm.Store() until the given id is present in
// the policy_rules bucket or timeout elapses.
func waitForRecord(t *testing.T, fsm *memory.FSM, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := fsm.Store().Get(memory.BucketPolicyRules, id); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("record %q didn't appear in fsm within %s", id, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
