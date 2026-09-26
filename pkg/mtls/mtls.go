package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc/credentials"
)

// NodeCreds holds everything the main lobslaw binary needs to run
// mTLS: this node's cert+key (presented to peers) and the cluster
// CA pool (used to verify peers). The CA private key is NEVER here.
//
// Certificate and trust pools are published together behind atomic.Pointer
// so Reload cannot expose a partially updated credential generation. In-flight
// handshakes retain their generation; new handshakes capture the current one.
type NodeCreds struct {
	caCertPath   string
	nodeCertPath string
	nodeKeyPath  string

	// Writers serialize to preserve the operator anchor across reloads.
	mu      sync.Mutex
	current atomic.Pointer[credentialState]
}

// credentialState is immutable after publication. A handshake captures one
// generation so its certificate and trust roots cannot straddle a reload.
type credentialState struct {
	certificate     *tls.Certificate
	clusterRoots    *x509.CertPool
	clientAuthRoots *x509.CertPool
	operatorCA      *x509.Certificate
	nodeID          string
}

// LoadNodeCreds reads the CA public cert and this node's cert+key,
// validates the node cert is signed by the CA, and returns a ready-
// to-use NodeCreds. Main container startup calls this.
//
// Fails fast if nodeCertPath is missing — this is the hook for the
// "run `lobslaw cluster sign-node` first" error in k8s initContainer
// flows.
func LoadNodeCreds(caCertPath, nodeCertPath, nodeKeyPath string) (*NodeCreds, error) {
	n := &NodeCreds{
		caCertPath:   caCertPath,
		nodeCertPath: nodeCertPath,
		nodeKeyPath:  nodeKeyPath,
	}
	if err := n.Reload(); err != nil {
		return nil, err
	}
	return n, nil
}

// Reload re-reads the CA + node cert + node key from disk, validates
// that the new node cert is signed by the (possibly updated) CA, and
// atomic-swaps both into the live config. New TLS handshakes after
// this returns will use the rotated material; in-flight handshakes
// are unaffected.
//
// Returns an error and leaves the current creds in place if anything
// fails — partial swap is forbidden (would leave the node serving
// with a cert it can't verify against its own CA pool).
func (n *NodeCreds) Reload() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, err := os.Stat(n.nodeCertPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("node cert %q does not exist — run `lobslaw cluster sign-node` first (typically as a k8s initContainer)", n.nodeCertPath)
		}
		return fmt.Errorf("stat node cert: %w", err)
	}

	caPEM, err := os.ReadFile(n.caCertPath)
	if err != nil {
		return fmt.Errorf("read CA cert %q: %w", n.caCertPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("CA cert PEM is invalid or empty")
	}

	nodeCert, err := tls.LoadX509KeyPair(n.nodeCertPath, n.nodeKeyPath)
	if err != nil {
		return fmt.Errorf("load node cert+key: %w", err)
	}

	leaf, err := x509.ParseCertificate(nodeCert.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse node cert: %w", err)
	}

	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return fmt.Errorf("node cert not signed by cluster CA at %q: %w", n.caCertPath, err)
	}

	nodeCert.Leaf = leaf
	next := &credentialState{certificate: &nodeCert, clusterRoots: pool, nodeID: leaf.Subject.CommonName}
	if previous := n.current.Load(); previous != nil {
		next.operatorCA = previous.operatorCA
	}
	next.clientAuthRoots = clientAuthRoots(pool, next.operatorCA)
	n.current.Store(next)
	return nil
}

// TrustOperatorCA adds a second anchor accepted for CLIENT
// certificates only.
//
// Additive, and it never touches RootCAs. An operator certificate is
// ClientAuth-only in any case, but keeping the pools apart means the
// guarantee does not rest on that alone.
func (n *NodeCreds) TrustOperatorCA(caPEM []byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	block, _ := pem.Decode(caPEM)
	if block == nil {
		return errors.New("operator CA PEM is invalid or empty")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse operator CA: %w", err)
	}
	if !cert.IsCA {
		return errors.New("operator CA certificate is not a CA")
	}
	next := *n.state()
	next.operatorCA = cert
	next.clientAuthRoots = clientAuthRoots(next.clusterRoots, cert)
	n.current.Store(&next)
	return nil
}

// OperatorCA returns the trusted operator root, or nil. Treat it as read-only.
func (n *NodeCreds) OperatorCA() *x509.Certificate { return n.state().operatorCA }

// NodeID returns the identity from the currently active certificate.
func (n *NodeCreds) NodeID() string { return n.state().nodeID }

func clientAuthRoots(cluster *x509.CertPool, operator *x509.Certificate) *x509.CertPool {
	if operator == nil {
		return cluster
	}
	merged := cluster.Clone()
	merged.AddCert(operator)
	return merged
}

// CAPool returns the cluster CA pool used to verify peers. Snapshot
// at last Reload — callers building their own tls.Config outside
// the gRPC path use this. The returned pool is read-only. Production paths should prefer ServerCreds
// / ClientCreds which capture both cert and pool together.
func (n *NodeCreds) CAPool() *x509.CertPool { return n.state().clusterRoots }

// Certificate returns a snapshot of the currently-active cert.
// Test/debug accessor — callers building their own tls.Config
// outside the gRPC path read this. Production hot-reload aware
// paths should use ServerCreds / ClientCreds instead so they pick
// up rotations automatically.
func (n *NodeCreds) Certificate() tls.Certificate {
	return *n.activeCert()
}

// activeCert returns the currently-loaded cert, panicking if Reload
// has never succeeded — callers always go through LoadNodeCreds
// which Reloads at construction, so this should be unreachable.
func (n *NodeCreds) activeCert() *tls.Certificate { return n.state().certificate }

func (n *NodeCreds) state() *credentialState {
	state := n.current.Load()
	if state == nil {
		panic("mtls: NodeCreds used before initial Reload (programmer error)")
	}
	return state
}

// ServerCreds returns gRPC TransportCredentials for an mTLS server.
// Clients must present a cert signed by the same cluster CA.
//
// Uses GetConfigForClient so a Reload mid-process picks up new material
// on the next handshake without bouncing the gRPC server.
func (n *NodeCreds) ServerCreds() credentials.TransportCredentials {
	return credentials.NewTLS(&tls.Config{
		GetCertificate: func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return n.activeCert(), nil
		},
		// Resolved per handshake so a later TrustOperatorCA or Reload
		// takes effect on the running server.
		GetConfigForClient: func(_ *tls.ClientHelloInfo) (*tls.Config, error) {
			state := n.state()
			return &tls.Config{
				Certificates: []tls.Certificate{*state.certificate},
				ClientCAs:    state.clientAuthRoots,
				ClientAuth:   tls.RequireAndVerifyClientCert,
				MinVersion:   tls.VersionTLS13,
			}, nil
		},
		ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS13,
	})
}

// EnrolmentServerConfig is TLS for the enrolment listener: this
// node's certificate, and NO client certificate required.
//
// The one surface that cannot demand one, because the caller is asking
// for the credential it would present. Kept as its own constructor so
// the relaxation is a named, greppable thing rather than a flag on the
// server config everything else uses.
func (n *NodeCreds) EnrolmentServerConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return n.activeCert(), nil
		},
		ClientAuth: tls.NoClientCert,
		MinVersion: tls.VersionTLS13,
	}
}

// ClientCreds returns gRPC TransportCredentials for an mTLS client.
// Verifies that the server presents a cert signed by the cluster CA.
//
// Loads one certificate/trust generation at each handshake, including when
// Raft retains this credential for the lifetime of the process.
func (n *NodeCreds) ClientCreds() credentials.TransportCredentials {
	return &clientCredentials{node: n}
}

// LoadClientCreds loads a credential for something that only ever
// DIALS.
//
// Distinct from LoadNodeCreds, which verifies the certificate it loads
// against the CA pool — correct for a node, whose identity must chain
// to the cluster CA, and wrong for an operator, whose certificate
// chains to the OPERATOR CA. Using the node loader for an operator
// credential fails with "certificate signed by unknown authority"
// before a single byte is sent, which is how an enrolled credential
// turned out to be unusable by the CLI that issued it.
//
// Verifying your own certificate is a sanity check, not a security
// control: the server is what must verify it, and the server holds
// both roots. What the client genuinely needs is RootCAs, so it can
// verify the NODE — and that is the cluster CA either way.
func LoadClientCreds(caCertPath, certPath, keyPath string) (credentials.TransportCredentials, error) {
	caPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("read CA cert %q: %w", caCertPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA cert %q is invalid or empty", caCertPath)
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load client cert+key: %w", err)
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{pair},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}), nil
}
