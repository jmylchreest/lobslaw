package mtls

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

// SignOpts configures node certificate signing.
type SignOpts struct {
	NodeID   string
	ValidFor time.Duration
	Now      time.Time // zero = time.Now()

	// DNSNames and IPs are the OTHER names this node answers at.
	//
	// A certificate is only valid for the names it carries, and the
	// node id alone is rarely one of them from outside the cluster: an
	// operator's laptop dialling "node1.example.com:9090" or
	// "10.0.0.4:9090" gets a certificate that verifies for neither.
	// Peers are fine either way, because they dial each other by id.
	DNSNames []string
	IPs      []net.IP
}

// LoadCA reads the CA cert + private key from disk. This function is
// only called by the `lobslaw cluster sign-node` subcommand — the main
// binary never invokes it.
func LoadCA(caCertPath, caKeyPath string) (*x509.Certificate, ed25519.PrivateKey, error) {
	certPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read CA cert %q: %w", caCertPath, err)
	}
	keyPEM, err := os.ReadFile(caKeyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read CA key %q: %w", caKeyPath, err)
	}

	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, nil, errors.New("CA cert PEM decode failed")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA cert: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "PRIVATE KEY" {
		return nil, nil, errors.New("CA key PEM decode failed")
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}
	key, ok := keyAny.(ed25519.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("CA key is not Ed25519 (got %T)", keyAny)
	}
	return cert, key, nil
}

// SignNodeCert issues a per-node certificate signed by the CA. Returns
// PEM-encoded cert + node key bytes. Callers write them to disk.
//
// The node ID is placed in the certificate's CommonName and SAN — peer
// identity at the gRPC layer comes from the SAN.
func SignNodeCert(caCert *x509.Certificate, caKey ed25519.PrivateKey, opts SignOpts) (certPEM, keyPEM []byte, err error) {
	if opts.NodeID == "" {
		return nil, nil, errors.New("NodeID required")
	}
	if opts.ValidFor == 0 {
		opts.ValidFor = 365 * 24 * time.Hour
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate node key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: opts.NodeID},
		NotBefore:    now,
		NotAfter:     now.Add(opts.ValidFor),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
		// The node id is always a SAN, because peers dial each other by
		// it. Everything else has to be declared: a certificate is only
		// valid for the names it carries, and a node reached at
		// "node1.example.com" or at an IP by an operator's laptop
		// presents one that verifies for neither.
		DNSNames:    append([]string{opts.NodeID}, opts.DNSNames...),
		IPAddresses: opts.IPs,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, pub, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("sign node cert: %w", err)
	}

	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal node key: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	return certPEM, keyPEM, nil
}

// WriteNodeFiles writes the node cert and key to disk.
// Cert is written 0o644; key 0o600. Parent directory must already exist.
func WriteNodeFiles(nodeCertPath, nodeKeyPath string, certPEM, keyPEM []byte) error {
	if err := os.WriteFile(nodeCertPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("write node cert: %w", err)
	}
	if err := os.WriteFile(nodeKeyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write node key: %w", err)
	}
	return nil
}

// WriteCAPublic copies just the CA public certificate to the node-certs
// directory so the main container can mount only that directory without
// ever seeing the CA private key.
func WriteCAPublic(dstCAPath string, caCertPEM []byte) error {
	if err := os.WriteFile(dstCAPath, caCertPEM, 0o644); err != nil {
		return fmt.Errorf("write CA public cert: %w", err)
	}
	return nil
}

// OperatorOU marks a certificate as belonging to a person rather than
// to a node.
//
// In the Subject's OrganizationalUnit rather than encoded into the
// CommonName, because the CN is already the identity every service
// reads for attribution — overloading it would make every reader parse
// a prefix, and the first reader that forgot would treat an operator
// as a node.
const OperatorOU = "operator"

// SignOperatorCert issues a credential for a PERSON.
//
// Distinct from a node certificate in two ways, and both matter.
//
// CLIENT AUTHENTICATION ONLY. A node cert carries ServerAuth as well,
// because a node both dials its peers and serves them; that is exactly
// what makes a stolen node credential able to impersonate a cluster
// member. An operator dials and is never dialled.
//
// AND MARKED AS AN OPERATOR, because ClientAuth alone does not stop it
// dialling the raft transport — a peer dials as a client too. The
// server refuses this OU on peer-only services, which is what makes
// "administers but cannot join" true rather than merely intended.
//
// A separate credential also makes revocation and audit answerable:
// revoking one person no longer means rotating a node's identity, and
// an audit entry names who rather than which host.
func SignOperatorCert(caCert *x509.Certificate, caKey ed25519.PrivateKey, opts SignOpts) (certPEM, keyPEM []byte, err error) {
	if opts.NodeID == "" {
		return nil, nil, errors.New("operator name required")
	}
	if opts.ValidFor == 0 {
		// Shorter than a node's year by default. A person's credential
		// lives on a laptop that travels; a node's lives on a host
		// somebody controls.
		opts.ValidFor = 90 * 24 * time.Hour
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate operator key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:         opts.NodeID,
			OrganizationalUnit: []string{OperatorOU},
		},
		NotBefore: now,
		NotAfter:  now.Add(opts.ValidFor),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		// No ServerAuth. This credential cannot be presented by
		// something answering connections.
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		// No DNS SAN either: an operator is not reachable at a name,
		// and a SAN is what a peer would verify when dialling.
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, pub, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("sign operator cert: %w", err)
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal operator key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}), nil
}

// IsOperatorCert reports whether a certificate belongs to a person.
func IsOperatorCert(cert *x509.Certificate) bool {
	if cert == nil {
		return false
	}
	for _, ou := range cert.Subject.OrganizationalUnit {
		if ou == OperatorOU {
			return true
		}
	}
	return false
}
