package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
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

	// clientAuthPool is what CLIENT certificates are verified against:
	// the cluster CA, plus the operator CA when one is trusted.
	//
	// Deliberately not the same pool as RootCAs. This node dialling a
	// peer must accept only the cluster CA, so an operator credential
	// can never be presented BY a server — which is the property that
	// keeps an online operator-signing key from being able to
	// manufacture a peer.
	clientAuthPool *x509.CertPool
	// operatorCA is retained so callers can ask which root actually
	// signed a client cert, rather than trusting an OU string.
	operatorCA *x509.Certificate

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
	// Rebuilt from the freshly-read cluster CA so a Reload cannot
	// silently drop the operator anchor — a rotation that logged
	// every operator out would be a confusing way to find out.
	n.rebuildClientAuthPool()
	return nil
}

// TrustOperatorCA adds a second anchor accepted for CLIENT
// certificates only.
//
// Additive, and it never touches RootCAs. An operator certificate is
// ClientAuth-only in any case, but keeping the pools apart means the
// guarantee does not rest on that alone.
func (n *NodeCreds) TrustOperatorCA(caPEM []byte) error {
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
	n.operatorCA = cert
	n.rebuildClientAuthPool()
	return nil
}

// OperatorCA returns the trusted operator root, or nil.
func (n *NodeCreds) OperatorCA() *x509.Certificate { return n.operatorCA }

func (n *NodeCreds) rebuildClientAuthPool() {
	if n.operatorCA == nil {
		n.clientAuthPool = n.pool
		return
	}
	// A fresh pool rather than AppendCertsFromPEM onto n.pool: mutating
	// the cluster pool in place would put the operator root into
	// RootCAs too, and that is exactly the mixing this design exists to
	// prevent.
	merged := x509.NewCertPool()
	for _, c := range n.poolCerts() {
		merged.AddCert(c)
	}
	merged.AddCert(n.operatorCA)
	n.clientAuthPool = merged
}

// poolCerts re-reads the cluster CA file so the merged pool can be
// built from certificates rather than from an opaque pool.
//
// x509.CertPool exposes no way to enumerate what is in it, so the
// alternative would be keeping a parallel slice in step with it by
// hand — one more thing to forget during a rotation.
func (n *NodeCreds) poolCerts() []*x509.Certificate {
	raw, err := os.ReadFile(n.caCertPath)
	if err != nil {
		return nil
	}
	var out []*x509.Certificate
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			return out
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if c, perr := x509.ParseCertificate(block.Bytes); perr == nil {
			out = append(out, c)
		}
	}
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
		ClientCAs:  n.clientAuthPool,
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
