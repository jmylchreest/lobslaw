package mtls

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"time"
)

// CAOpts configures CA generation.
type CAOpts struct {
	CommonName string
	ValidFor   time.Duration
	Now        time.Time // overridable for deterministic tests; zero = time.Now()
}

// GenerateCA creates a self-signed Ed25519 CA certificate and private key.
// Returns PEM-encoded cert and key bytes. Callers write them to disk.
func GenerateCA(opts CAOpts) (certPEM, keyPEM []byte, err error) {
	if opts.CommonName == "" {
		opts.CommonName = "Lobslaw Cluster CA"
	}
	if opts.ValidFor == 0 {
		opts.ValidFor = 10 * 365 * 24 * time.Hour
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: opts.CommonName},
		NotBefore:             now,
		NotAfter:              now.Add(opts.ValidFor),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("sign CA cert: %w", err)
	}

	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal CA key: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	return certPEM, keyPEM, nil
}

// WriteCAFiles writes the CA cert and key to disk. Refuses to overwrite
// existing files — operators must delete + regenerate deliberately.
// The key file is written with mode 0o600; the cert with 0o644.
func WriteCAFiles(caCertPath, caKeyPath string, certPEM, keyPEM []byte) error {
	for _, p := range []string{caCertPath, caKeyPath} {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("refusing to overwrite existing file %q", p)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat %q: %w", p, err)
		}
	}
	if err := os.WriteFile(caCertPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("write CA cert: %w", err)
	}
	if err := os.WriteFile(caKeyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write CA key: %w", err)
	}
	return nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	return serial, nil
}
