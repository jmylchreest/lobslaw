package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync/atomic"

	"google.golang.org/grpc/credentials"
)

// NodeCreds holds everything the main lobslaw binary needs to run
// mTLS: this node's cert+key (presented to peers) and the cluster
// CA pool (used to verify peers). The CA private key is NEVER here.
//
// The active certificate is held behind atomic.Pointer so Reload
// can atomically swap it during cert rotation. Existing in-flight
// handshakes complete with the previous cert; new handshakes after
// the swap pick up the rotated material via the GetCertificate
// callback. Goroutine-safe by construction.
type NodeCreds struct {
	caCertPath   string
	nodeCertPath string
	nodeKeyPath  string

	active atomic.Pointer[tls.Certificate]
	pool   *x509.CertPool

	// NodeID is the CommonName from the node cert at last load. Read
	// without locking — only updated by Reload, which is single-writer.
	NodeID string
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
	n.active.Store(&nodeCert)
	n.pool = pool
	n.NodeID = leaf.Subject.CommonName
	return nil
}

// CAPool returns the cluster CA pool used to verify peers. Snapshot
// at last Reload — callers building their own tls.Config outside
// the gRPC path use this. Production paths should prefer ServerCreds
// / ClientCreds which capture both cert and pool together.
func (n *NodeCreds) CAPool() *x509.CertPool { return n.pool }

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
func (n *NodeCreds) activeCert() *tls.Certificate {
	c := n.active.Load()
	if c == nil {
		panic("mtls: NodeCreds used before initial Reload (programmer error)")
	}
	return c
}

// ServerCreds returns gRPC TransportCredentials for an mTLS server.
// Clients must present a cert signed by the same cluster CA.
//
// Uses GetCertificate so a Reload mid-process picks up new material
// on the next handshake without bouncing the gRPC server.
func (n *NodeCreds) ServerCreds() credentials.TransportCredentials {
	return credentials.NewTLS(&tls.Config{
		GetCertificate: func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return n.activeCert(), nil
		},
		ClientCAs:  n.pool,
		ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS13,
	})
}

// ClientCreds returns gRPC TransportCredentials for an mTLS client.
// Verifies that the server presents a cert signed by the cluster CA.
//
// Uses GetClientCertificate for the same hot-reload reason as
// ServerCreds.
func (n *NodeCreds) ClientCreds() credentials.TransportCredentials {
	return credentials.NewTLS(&tls.Config{
		GetClientCertificate: func(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return n.activeCert(), nil
		},
		RootCAs:    n.pool,
		MinVersion: tls.VersionTLS13,
	})
}
