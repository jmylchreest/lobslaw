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
	"path/filepath"
	"time"
)

// A SECOND trust anchor, for people rather than machines.
//
// The obvious design was an intermediate under the cluster CA. It does
// not work here and could not be made to without a migration:
// GenerateCA emits MaxPathLen 0, so the cluster CA can sign end-entity
// certificates and nothing else. Go rejects a chain through it with
// "too many intermediates for path length constraint". Raising the
// path length means reissuing the root, which means reissuing every
// node certificate.
//
// So operator certificates chain to their own root, and the node holds
// that root's key. The security property is the one an intermediate
// would have given, arrived at more directly:
//
//   - the online key can mint OPERATOR credentials, which are
//     ClientAuth-only and refused on the raft transport
//   - it cannot mint a NODE credential, because peers verify each
//     other against the CLUSTER CA and this key is not in that chain
//
// A compromised node therefore hands an attacker the ability to
// impersonate operators — bad, and bounded — but not the ability to
// manufacture a peer, which is what would let one compromise become
// an unbounded number of replicas holding a full copy of everything.
//
// The separation is structural rather than attribute-based. IsOperatorCert
// reads an OU string; ChainsToOperatorCA reads which root actually
// signed the thing. The second is not forgeable by naming yourself.

// OperatorCACommonName identifies the operator root in a listing, so
// somebody looking at two CA files can tell which is which.
const OperatorCACommonName = "lobslaw-operator-ca"

// GenerateOperatorCA creates the self-signed root that signs operator
// certificates.
//
// MaxPathLen 0: this root signs people, never other CAs. It is the
// same constraint that made an intermediate impossible under the
// cluster CA, applied deliberately here — an operator root that could
// delegate would be a way to smuggle the authority somewhere else.
func GenerateOperatorCA(validFor time.Duration) (certPEM, keyPEM []byte, err error) {
	if validFor <= 0 {
		validFor = 10 * 365 * 24 * time.Hour
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate operator CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:         OperatorCACommonName,
			OrganizationalUnit: []string{OperatorOU},
		},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(validFor),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("self-sign operator CA: %w", err)
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal operator CA key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}), nil
}

// LoadOrCreateOperatorCA reads the operator root, creating it on first
// use.
//
// Created rather than required, because an operator CA is not a thing
// an administrator should have to know about before their first
// enrolment. It is generated with 0600 on the key, in the node's own
// directory, and it never leaves.
func LoadOrCreateOperatorCA(certPath, keyPath string) (*x509.Certificate, ed25519.PrivateKey, error) {
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	switch {
	case certErr == nil && keyErr == nil:
		return LoadCA(certPath, keyPath)
	case certErr == nil || keyErr == nil:
		// Exactly one half present. Refused rather than regenerated:
		// overwriting a surviving cert would invalidate every operator
		// credential already issued under it, and doing that silently
		// during a routine boot is not recoverable from the logs.
		return nil, nil, fmt.Errorf(
			"operator CA is half-present (cert=%v key=%v); refusing to regenerate over it",
			certErr == nil, keyErr == nil)
	}

	certPEM, keyPEM, err := GenerateOperatorCA(0)
	if err != nil {
		return nil, nil, err
	}
	if mkErr := os.MkdirAll(filepath.Dir(certPath), 0o750); mkErr != nil {
		return nil, nil, fmt.Errorf("create operator CA dir: %w", mkErr)
	}
	if werr := WriteCAFiles(certPath, keyPath, certPEM, keyPEM); werr != nil {
		return nil, nil, werr
	}
	return LoadCA(certPath, keyPath)
}

// SignOperatorCSR issues an operator certificate for a key the CALLER
// already holds.
//
// This is the whole point of enrolment: the request carries a public
// key and a name, and the private half never exists on this machine.
// SignOperatorCert, which generates the keypair here, remains for the
// bootstrap case where there is nobody to approve an enrolment yet.
//
// The name comes from the caller's argument, NOT from the CSR's
// subject. A CSR is attacker-shaped input and its subject is whatever
// the requester typed; the name that ends up in the certificate is the
// one the approver saw and agreed to.
func SignOperatorCSR(caCert *x509.Certificate, caKey ed25519.PrivateKey,
	csr *x509.CertificateRequest, name string, validFor time.Duration) (certPEM []byte, err error) {
	switch {
	case caCert == nil || caKey == nil:
		return nil, errors.New("operator CA is required")
	case csr == nil:
		return nil, errors.New("certificate request is required")
	case name == "":
		return nil, errors.New("operator name required")
	}
	if err := csr.CheckSignature(); err != nil {
		// Proves the requester holds the private half of the key they
		// are asking us to certify. Without it anyone could enrol
		// somebody else's public key and then be unable to use it —
		// or, worse, get a certificate issued in a name they chose for
		// a key somebody else controls.
		return nil, fmt.Errorf("certificate request is not self-signed: %w", err)
	}
	pub, ok := csr.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("operator keys are ed25519; this request carries %T", csr.PublicKey)
	}
	if validFor <= 0 {
		validFor = 90 * 24 * time.Hour
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:         name,
			OrganizationalUnit: []string{OperatorOU},
		},
		NotBefore: now.Add(-time.Minute),
		NotAfter:  now.Add(validFor),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		// The same two refusals SignOperatorCert makes, for the same
		// reasons: no ServerAuth, so nothing can answer connections
		// with this; no DNS SAN, so it is not addressable as a host.
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, pub, caKey)
	if err != nil {
		return nil, fmt.Errorf("sign operator CSR: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// ChainsToOperatorCA reports whether a verified chain terminates at
// the operator root.
//
// The structural counterpart to IsOperatorCert. That one reads an OU
// string, which is whatever the subject says; this reads which key
// actually signed the certificate, which is not forgeable by naming
// yourself.
func ChainsToOperatorCA(chains [][]*x509.Certificate, operatorCA *x509.Certificate) bool {
	if operatorCA == nil {
		return false
	}
	for _, chain := range chains {
		if len(chain) == 0 {
			continue
		}
		root := chain[len(chain)-1]
		if root.Equal(operatorCA) {
			return true
		}
	}
	return false
}
