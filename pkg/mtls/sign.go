package mtls

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"
)

// SignOpts configures node certificate signing.
type SignOpts struct {
	NodeID   string
	ValidFor time.Duration
	Now      time.Time // zero = time.Now()
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
		DNSNames: []string{opts.NodeID},
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
