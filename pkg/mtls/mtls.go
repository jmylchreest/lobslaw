package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
)

// NodeCreds holds everything the main lobslaw binary needs to run
// mTLS: this node's cert+key (presented to peers) and the cluster
// CA pool (used to verify peers). The CA private key is NEVER here.
type NodeCreds struct {
	NodeCert tls.Certificate
	CAPool   *x509.CertPool
	NodeID   string // CommonName from the node cert, extracted for logging/audit
}

// LoadNodeCreds reads the CA public cert and this node's cert+key,
// validates the node cert is signed by the CA, and returns a ready-
// to-use NodeCreds. Main container startup calls this.
//
// Fails fast if nodeCertPath is missing — this is the hook for the
// "run `lobslaw cluster sign-node` first" error in k8s initContainer
// flows.
func LoadNodeCreds(caCertPath, nodeCertPath, nodeKeyPath string) (*NodeCreds, error) {
	if _, err := os.Stat(nodeCertPath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("node cert %q does not exist — run `lobslaw cluster sign-node` first (typically as a k8s initContainer)", nodeCertPath)
		}
		return nil, fmt.Errorf("stat node cert: %w", err)
	}

	caPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("read CA cert %q: %w", caCertPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("CA cert PEM is invalid or empty")
	}

	nodeCert, err := tls.LoadX509KeyPair(nodeCertPath, nodeKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load node cert+key: %w", err)
	}

	leaf, err := x509.ParseCertificate(nodeCert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse node cert: %w", err)
	}

	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return nil, fmt.Errorf("node cert not signed by cluster CA at %q: %w", caCertPath, err)
	}

	nodeCert.Leaf = leaf
	return &NodeCreds{
		NodeCert: nodeCert,
		CAPool:   pool,
		NodeID:   leaf.Subject.CommonName,
	}, nil
}

// ServerCreds returns gRPC TransportCredentials for an mTLS server.
// Clients must present a cert signed by the same cluster CA.
func (n *NodeCreds) ServerCreds() credentials.TransportCredentials {
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{n.NodeCert},
		ClientCAs:    n.CAPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	})
}

// ClientCreds returns gRPC TransportCredentials for an mTLS client.
// Verifies that the server presents a cert signed by the cluster CA.
func (n *NodeCreds) ClientCreds() credentials.TransportCredentials {
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{n.NodeCert},
		RootCAs:      n.CAPool,
		MinVersion:   tls.VersionTLS13,
	})
}
