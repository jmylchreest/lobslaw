package mtls

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The obvious design was an intermediate under the cluster CA. It does
// not work: GenerateCA emits MaxPathLen 0, so the cluster CA signs
// end-entity certificates and nothing else.
//
// So operator certificates chain to their own root, and the node holds
// that root's key. These tests hold that arrangement to the property
// an intermediate would have given — the online key can mint people,
// never peers.

func operatorCA(t *testing.T) (*x509.Certificate, ed25519.PrivateKey) {
	t.Helper()
	certPEM, keyPEM, err := GenerateOperatorCA(0)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "operator-ca.pem")
	keyPath := filepath.Join(dir, "operator-ca-key.pem")
	if err := WriteCAFiles(certPath, keyPath, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	ca, key, err := LoadCA(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return ca, key
}

// newCSR builds a request the way an enrolling laptop would: the key
// is generated here and only the public half goes into the request.
func newCSR(t *testing.T, subject string) (*x509.CertificateRequest, ed25519.PrivateKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: subject}}, priv)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		t.Fatal(err)
	}
	return csr, priv
}

// THE PROPERTY THE WHOLE DESIGN RESTS ON. The cluster CA is what peers
// verify each other against; a certificate signed by the operator root
// must not verify there, or an online key could manufacture a peer.
func TestAnOperatorCertDoesNotVerifyAgainstTheClusterCA(t *testing.T) {
	t.Parallel()
	clusterCA, _ := testCA(t)
	opCA, opKey := operatorCA(t)
	csr, _ := newCSR(t, "alice")

	certPEM, err := SignOperatorCSR(opCA, opKey, csr, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(clusterCA)
	if _, verr := parseCert(t, certPEM).Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); verr == nil {
		t.Fatal("an operator certificate verified against the CLUSTER CA; " +
			"an online operator-signing key could manufacture a peer")
	}
}

// And it does verify against its own root, or nothing works.
func TestAnOperatorCertVerifiesAgainstTheOperatorCA(t *testing.T) {
	t.Parallel()
	opCA, opKey := operatorCA(t)
	csr, _ := newCSR(t, "alice")

	certPEM, err := SignOperatorCSR(opCA, opKey, csr, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(opCA)
	if _, verr := parseCert(t, certPEM).Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); verr != nil {
		t.Errorf("an operator certificate does not verify against its own root: %v", verr)
	}
}

// The operator root signs people, never other CAs. A root that could
// delegate would be a way to smuggle the authority somewhere else.
func TestTheOperatorCACannotDelegate(t *testing.T) {
	t.Parallel()
	opCA, _ := operatorCA(t)
	if !opCA.MaxPathLenZero || opCA.MaxPathLen != 0 {
		t.Errorf("operator CA path length = %d (zero=%v); it can issue subordinate CAs",
			opCA.MaxPathLen, opCA.MaxPathLenZero)
	}
}

// Issued certificates keep the two refusals SignOperatorCert makes.
func TestACSRSignedCertCannotServeAndIsNotAddressable(t *testing.T) {
	t.Parallel()
	opCA, opKey := operatorCA(t)
	csr, _ := newCSR(t, "alice")

	certPEM, err := SignOperatorCSR(opCA, opKey, csr, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	cert := parseCert(t, certPEM)
	for _, u := range cert.ExtKeyUsage {
		if u == x509.ExtKeyUsageServerAuth {
			t.Error("a CSR-signed operator cert carries ServerAuth; it could answer connections")
		}
	}
	if len(cert.DNSNames) != 0 {
		t.Errorf("DNSNames = %v; an operator is not reachable at a name", cert.DNSNames)
	}
	if !IsOperatorCert(cert) {
		t.Error("a CSR-signed cert is not recognisable as an operator's")
	}
}

// THE KEY NEVER MOVES. The certificate must certify the key the
// requester already holds — if it certified a different one the whole
// point of enrolment is lost.
func TestTheIssuedCertCertifiesTheRequestersOwnKey(t *testing.T) {
	t.Parallel()
	opCA, opKey := operatorCA(t)
	csr, priv := newCSR(t, "alice")

	certPEM, err := SignOperatorCSR(opCA, opKey, csr, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := parseCert(t, certPEM).PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatal("issued cert does not carry an ed25519 key")
	}
	if !got.Equal(priv.Public()) {
		t.Error("the certificate certifies a different key than the requester holds")
	}
}

// The name comes from the APPROVER's argument, not the CSR's subject.
// A CSR is attacker-shaped input and its subject is whatever the
// requester typed; the name in the certificate has to be the one the
// approver saw and agreed to.
func TestTheSubjectComesFromTheApproverNotTheRequest(t *testing.T) {
	t.Parallel()
	opCA, opKey := operatorCA(t)
	csr, _ := newCSR(t, "root")

	certPEM, err := SignOperatorCSR(opCA, opKey, csr, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := parseCert(t, certPEM).Subject.CommonName; got != "alice" {
		t.Errorf("CommonName = %q; the requester chose their own name", got)
	}
}

// Proof of possession. Without it somebody could enrol a public key
// they do not control — getting a certificate issued in a name they
// chose, for a key somebody else holds.
func TestACSRWithABrokenSignatureIsRefused(t *testing.T) {
	t.Parallel()
	opCA, opKey := operatorCA(t)
	csr, _ := newCSR(t, "alice")
	// Corrupt the signature without touching anything else.
	csr.Signature[0] ^= 0xff

	if _, err := SignOperatorCSR(opCA, opKey, csr, "alice", 0); err == nil {
		t.Fatal("a request that does not prove key possession was signed")
	}
}

func TestSigningNeedsANameAndARequest(t *testing.T) {
	t.Parallel()
	opCA, opKey := operatorCA(t)
	csr, _ := newCSR(t, "alice")

	if _, err := SignOperatorCSR(opCA, opKey, csr, "", 0); err == nil {
		t.Error("an unnamed operator was issued a certificate")
	}
	if _, err := SignOperatorCSR(opCA, opKey, nil, "alice", 0); err == nil {
		t.Error("a nil request was signed")
	}
	if _, err := SignOperatorCSR(nil, nil, csr, "alice", 0); err == nil {
		t.Error("signing succeeded with no CA")
	}
}

// --- persistence -------------------------------------------------------

// Created on first use: an operator CA is not something an
// administrator should have to know about before their first
// enrolment.
func TestTheOperatorCAIsCreatedOnFirstUse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "operator-ca.pem")
	keyPath := filepath.Join(dir, "operator-ca-key.pem")

	ca, key, err := LoadOrCreateOperatorCA(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if ca == nil || key == nil {
		t.Fatal("no CA produced")
	}

	// And a second call returns the SAME root, or every restart would
	// invalidate every credential issued before it.
	again, _, err := LoadOrCreateOperatorCA(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Equal(ca) {
		t.Error("a second load produced a different root; existing operators would be locked out")
	}
}

// Overwriting a surviving cert would invalidate every credential
// already issued under it, and doing that silently during a routine
// boot is not recoverable from the logs.
func TestAHalfPresentOperatorCAIsRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "operator-ca.pem")
	keyPath := filepath.Join(dir, "operator-ca-key.pem")

	if _, _, err := LoadOrCreateOperatorCA(certPath, keyPath); err != nil {
		t.Fatal(err)
	}
	if err := removeFile(keyPath); err != nil {
		t.Fatal(err)
	}

	_, _, err := LoadOrCreateOperatorCA(certPath, keyPath)
	if err == nil {
		t.Fatal("a half-present operator CA was silently regenerated")
	}
	// And it says WHICH condition. WriteCAFiles refuses to overwrite
	// too, so removing this guard still errors — with "refusing to
	// overwrite existing file", which describes the symptom rather
	// than the situation. "Half-present" is what somebody needs to
	// read to know they have lost the key to a live root.
	if !strings.Contains(err.Error(), "half-present") {
		t.Errorf("error %q does not name the condition", err)
	}
}

// --- which root actually signed it -------------------------------------

// The structural counterpart to IsOperatorCert: that one reads an OU
// string, which is whatever the subject says. This reads which key
// signed the thing, which is not forgeable by naming yourself.
func TestChainsToOperatorCAReadsTheRootNotTheName(t *testing.T) {
	t.Parallel()
	clusterCA, clusterKey := testCA(t)
	opCA, _ := operatorCA(t)

	// A NODE certificate that calls itself an operator. IsOperatorCert
	// would be fooled by the OU alone; the chain root cannot be.
	impostorPEM, _, err := SignOperatorCert(clusterCA, clusterKey, SignOpts{NodeID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	impostor := parseCert(t, impostorPEM)
	pool := x509.NewCertPool()
	pool.AddCert(clusterCA)
	chains, err := impostor.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !IsOperatorCert(impostor) {
		t.Fatal("precondition: the impostor should carry the operator OU")
	}
	if ChainsToOperatorCA(chains, opCA) {
		t.Error("a cluster-CA-signed cert was reported as chaining to the operator root")
	}
}

func TestChainsToOperatorCARecognisesItsOwn(t *testing.T) {
	t.Parallel()
	opCA, opKey := operatorCA(t)
	csr, _ := newCSR(t, "alice")
	certPEM, err := SignOperatorCSR(opCA, opKey, csr, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(opCA)
	chains, err := parseCert(t, certPEM).Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ChainsToOperatorCA(chains, opCA) {
		t.Error("an operator-CA-signed cert was not recognised as one")
	}
	if ChainsToOperatorCA(chains, nil) {
		t.Error("a nil operator CA matched something")
	}
}

func removeFile(path string) error { return os.Remove(path) }

// --- the pool separation -----------------------------------------------

// nodeCredsWith builds NodeCreds against a real cluster CA on disk.
func nodeCredsWith(t *testing.T) (*NodeCreds, *x509.Certificate) {
	t.Helper()
	dir := t.TempDir()
	caCertPath := filepath.Join(dir, "ca.pem")
	caKeyPath := filepath.Join(dir, "ca-key.pem")
	certPEM, keyPEM, err := GenerateCA(CAOpts{CommonName: "cluster-ca"})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCAFiles(caCertPath, caKeyPath, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	caCert, caKey, err := LoadCA(caCertPath, caKeyPath)
	if err != nil {
		t.Fatal(err)
	}

	nodeCertPEM, nodeKeyPEM, err := SignNodeCert(caCert, caKey, SignOpts{NodeID: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	nodeCertPath := filepath.Join(dir, "node.pem")
	nodeKeyPath := filepath.Join(dir, "node-key.pem")
	if err := os.WriteFile(nodeCertPath, nodeCertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nodeKeyPath, nodeKeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	creds, err := LoadNodeCreds(caCertPath, nodeCertPath, nodeKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	return creds, caCert
}

// Trusting the operator root must not put it into RootCAs. This node
// dialling a peer has to accept only the cluster CA — that is the
// property that stops an online operator-signing key from being able
// to stand up something a peer would talk to.
func TestTrustingTheOperatorCADoesNotWidenRootCAs(t *testing.T) {
	t.Parallel()
	creds, clusterCA := nodeCredsWith(t)
	opCertPEM, _, err := GenerateOperatorCA(0)
	if err != nil {
		t.Fatal(err)
	}
	before := creds.CAPool()

	if err := creds.TrustOperatorCA(opCertPEM); err != nil {
		t.Fatal(err)
	}

	if creds.CAPool() != before {
		t.Error("trusting an operator CA replaced the peer-verification pool")
	}
	if !creds.CAPool().Equal(poolOf(clusterCA)) {
		t.Error("the peer-verification pool is no longer exactly the cluster CA")
	}
	if creds.OperatorCA() == nil {
		t.Error("the operator CA was not retained for chain checks")
	}
}

// A reload must not drop the operator anchor — a cert rotation that
// logged every operator out would be a confusing way to find out.
func TestAReloadKeepsTheOperatorAnchor(t *testing.T) {
	t.Parallel()
	creds, _ := nodeCredsWith(t)
	opCertPEM, _, err := GenerateOperatorCA(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.TrustOperatorCA(opCertPEM); err != nil {
		t.Fatal(err)
	}

	if err := creds.Reload(); err != nil {
		t.Fatal(err)
	}
	if creds.OperatorCA() == nil {
		t.Fatal("a reload dropped the operator anchor; every operator would be locked out")
	}
}

func TestGarbageIsNotAnOperatorCA(t *testing.T) {
	t.Parallel()
	creds, _ := nodeCredsWith(t)
	if err := creds.TrustOperatorCA([]byte("not a pem")); err == nil {
		t.Error("garbage was accepted as an operator CA")
	}
	// A leaf certificate is not a CA, and trusting one as an anchor
	// would let its holder issue credentials.
	opCA, opKey := operatorCA(t)
	csr, _ := newCSR(t, "alice")
	leafPEM, err := SignOperatorCSR(opCA, opKey, csr, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.TrustOperatorCA(leafPEM); err == nil {
		t.Error("a leaf certificate was accepted as a CA")
	}
}

func poolOf(certs ...*x509.Certificate) *x509.CertPool {
	p := x509.NewCertPool()
	for _, c := range certs {
		p.AddCert(c)
	}
	return p
}

// --- a real handshake --------------------------------------------------

// The pool-shape assertions above check the plumbing. This checks the
// thing that actually matters: can a laptop holding an operator
// certificate complete an mTLS handshake with this node, and can one
// holding nothing the node trusts fail to?
//
// Worth the extra machinery. Two mutations survived the field-level
// tests — the server config reading the wrong pool, and a Reload
// dropping the anchor — because both leave the fields I was asserting
// on untouched and only show up in a handshake.
func handshake(t *testing.T, server *NodeCreds, clientCert tls.Certificate, clusterCA *x509.Certificate) error {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	errc := make(chan error, 1)
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			errc <- aerr
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, _, herr := server.ServerCreds().ServerHandshake(conn)
		errc <- herr
	}()

	raw, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))

	roots := x509.NewCertPool()
	roots.AddCert(clusterCA)
	tc := tls.Client(raw, &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      roots,
		ServerName:   "node-a",
		MinVersion:   tls.VersionTLS13,
		// gRPC's ServerHandshake negotiates ALPN; without this the
		// two sides never agree and the handshake stalls rather than
		// failing.
		NextProtos: []string{"h2"},
	})
	clientErr := tc.HandshakeContext(t.Context())
	serverErr := <-errc

	// The server sees the client-cert rejection; the client often just
	// sees the connection close. Prefer whichever is non-nil.
	if serverErr != nil {
		return serverErr
	}
	return clientErr
}

// operatorClientCert issues one against the given operator root.
func operatorClientCert(t *testing.T, ca *x509.Certificate, key ed25519.PrivateKey) tls.Certificate {
	t.Helper()
	csr, priv := newCSR(t, "alice")
	certPEM, err := SignOperatorCSR(ca, key, csr, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

func TestAnOperatorCertCompletesAnMTLSHandshake(t *testing.T) {
	t.Parallel()
	creds, clusterCA := nodeCredsWith(t)
	opCA, opKey := operatorCA(t)
	if err := creds.TrustOperatorCA(pemOf(t, opCA)); err != nil {
		t.Fatal(err)
	}

	if err := handshake(t, creds, operatorClientCert(t, opCA, opKey), clusterCA); err != nil {
		t.Fatalf("an enrolled operator could not connect: %v", err)
	}
}

// An operator root the node does NOT trust must be refused, or
// "trusting" one means nothing.
func TestAnUntrustedOperatorRootIsRefused(t *testing.T) {
	t.Parallel()
	creds, clusterCA := nodeCredsWith(t)
	trusted, trustedKey := operatorCA(t)
	if err := creds.TrustOperatorCA(pemOf(t, trusted)); err != nil {
		t.Fatal(err)
	}
	stranger, strangerKey := operatorCA(t)

	if err := handshake(t, creds, operatorClientCert(t, stranger, strangerKey), clusterCA); err == nil {
		t.Fatal("a certificate from an untrusted operator root completed the handshake")
	}
	// And the trusted one still works, so the refusal is not just
	// everything failing.
	if err := handshake(t, creds, operatorClientCert(t, trusted, trustedKey), clusterCA); err != nil {
		t.Fatalf("the trusted root stopped working: %v", err)
	}
}

// A cert rotation must not log every operator out.
func TestOperatorsSurviveANodeCertReload(t *testing.T) {
	t.Parallel()
	creds, clusterCA := nodeCredsWith(t)
	opCA, opKey := operatorCA(t)
	if err := creds.TrustOperatorCA(pemOf(t, opCA)); err != nil {
		t.Fatal(err)
	}
	if err := creds.Reload(); err != nil {
		t.Fatal(err)
	}

	if err := handshake(t, creds, operatorClientCert(t, opCA, opKey), clusterCA); err != nil {
		t.Fatalf("a node cert reload locked every operator out: %v", err)
	}
}

// A cluster CA ROTATION must carry both anchors forward.
//
// The weaker test above reloads the same CA, which cannot tell whether
// the client pool was rebuilt or merely left alone — it looks identical
// either way. Rotating exposes it: a stale client pool still holds the
// OLD cluster CA, so peers presenting freshly-signed certificates get
// refused while everything else looks healthy.
func TestACARotationKeepsBothAnchors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	caCertPath := filepath.Join(dir, "ca.pem")
	caKeyPath := filepath.Join(dir, "ca-key.pem")
	nodeCertPath := filepath.Join(dir, "node.pem")
	nodeKeyPath := filepath.Join(dir, "node-key.pem")

	writeCluster := func(cn string) (*x509.Certificate, ed25519.PrivateKey) {
		t.Helper()
		for _, p := range []string{caCertPath, caKeyPath, nodeCertPath, nodeKeyPath} {
			_ = os.Remove(p)
		}
		certPEM, keyPEM, err := GenerateCA(CAOpts{CommonName: cn})
		if err != nil {
			t.Fatal(err)
		}
		if werr := WriteCAFiles(caCertPath, caKeyPath, certPEM, keyPEM); werr != nil {
			t.Fatal(werr)
		}
		ca, key, err := LoadCA(caCertPath, caKeyPath)
		if err != nil {
			t.Fatal(err)
		}
		nc, nk, err := SignNodeCert(ca, key, SignOpts{NodeID: "node-a"})
		if err != nil {
			t.Fatal(err)
		}
		if werr := os.WriteFile(nodeCertPath, nc, 0o600); werr != nil {
			t.Fatal(werr)
		}
		if werr := os.WriteFile(nodeKeyPath, nk, 0o600); werr != nil {
			t.Fatal(werr)
		}
		return ca, key
	}

	writeCluster("cluster-ca-v1")
	creds, err := LoadNodeCreds(caCertPath, nodeCertPath, nodeKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	opCA, opKey := operatorCA(t)
	if terr := creds.TrustOperatorCA(pemOf(t, opCA)); terr != nil {
		t.Fatal(terr)
	}

	// Rotate to a second, unrelated cluster CA.
	ca2, ca2Key := writeCluster("cluster-ca-v2")
	if rerr := creds.Reload(); rerr != nil {
		t.Fatal(rerr)
	}

	// A peer holding a v2-signed certificate must be accepted, or the
	// rotation has quietly partitioned the cluster.
	peerCertPEM, peerKeyPEM, err := SignNodeCert(ca2, ca2Key, SignOpts{NodeID: "node-b"})
	if err != nil {
		t.Fatal(err)
	}
	peer, err := tls.X509KeyPair(peerCertPEM, peerKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if herr := handshake(t, creds, peer, ca2); herr != nil {
		t.Errorf("a peer signed by the rotated CA was refused: %v", herr)
	}

	// And the operator anchor came through the rotation with it.
	if herr := handshake(t, creds, operatorClientCert(t, opCA, opKey), ca2); herr != nil {
		t.Errorf("a CA rotation locked every operator out: %v", herr)
	}
}

func pemOf(t *testing.T, c *x509.Certificate) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
}
