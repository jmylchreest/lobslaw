package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"

	"github.com/hashicorp/raft"
)

// The seam between the request queue and the operator CA. Everything
// here is about what an UNAUTHENTICATED caller may do, and what an
// authenticated one must prove.

func newEnrolmentSvc(t *testing.T) *enrolmentService {
	t.Helper()
	dir := t.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(dir, "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	_, inmem := raft.NewInmemTransport("enrol-node")
	node, err := memory.NewRaft(memory.RaftConfig{
		NodeID: "enrol-node", LocalAddr: "enrol-node",
		DataDir: dir, Bootstrap: true, Transport: inmem,
	}, memory.NewFSM(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = node.Shutdown()
		_ = store.Close()
	})
	if err := node.WaitForLeader(5 * time.Second); err != nil {
		t.Fatal(err)
	}

	es, err := memory.NewEnrolmentStore(memory.EnrolmentStoreConfig{Raft: node, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	caCert, caKey, err := mtls.LoadOrCreateOperatorCA(
		filepath.Join(dir, "operator-ca.pem"), filepath.Join(dir, "operator-ca-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	return &enrolmentService{
		store: es, caCert: caCert, caKey: caKey,
		clusterCA: []byte("cluster-ca-pem"), operatorCA: []byte("operator-ca-pem"),
	}
}

// operatorCtx builds a context carrying a VERIFIED client certificate,
// which is the only thing DecideEnrolment will read an identity from.
//
// Constructed rather than faked with a plain string: the service reads
// the verified chain on purpose, and a test that bypassed that would
// not be testing the thing that matters.
func operatorCtx(t *testing.T, name string) context.Context {
	t.Helper()
	dir := t.TempDir()
	certPEM, keyPEM, err := mtls.GenerateCA(mtls.CAOpts{CommonName: "test-ca"})
	if err != nil {
		t.Fatal(err)
	}
	if werr := mtls.WriteCAFiles(filepath.Join(dir, "ca.pem"),
		filepath.Join(dir, "ca-key.pem"), certPEM, keyPEM); werr != nil {
		t.Fatal(werr)
	}
	caCert, caKey, err := mtls.LoadCA(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	opPEM, _, err := mtls.SignOperatorCert(caCert, caKey, mtls.SignOpts{NodeID: name})
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(opPEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
			VerifiedChains: [][]*x509.Certificate{{leaf, caCert}},
		}},
	})
}

func testCSR(t *testing.T, name string) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: name}}, priv)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func submitTo(t *testing.T, svc *enrolmentService, name string) *lobslawv1.SubmitEnrolmentResponse {
	t.Helper()
	res, err := svc.SubmitEnrolment(context.Background(), &lobslawv1.SubmitEnrolmentRequest{
		RequestedName: name, CsrDer: testCSR(t, name),
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// --- what an unauthenticated caller may do -----------------------------

func TestSubmitNeedsNoCredential(t *testing.T) {
	t.Parallel()
	svc := newEnrolmentSvc(t)

	res := submitTo(t, svc, "alice")
	if res.GetId() == "" || res.GetFingerprint() == "" {
		t.Fatalf("submit returned %+v", res)
	}
}

// A malformed request is the caller's mistake, and saying so beats a
// 500 that reads as a broken cluster.
func TestAMalformedSubmissionIsInvalidArgument(t *testing.T) {
	t.Parallel()
	svc := newEnrolmentSvc(t)

	_, err := svc.SubmitEnrolment(context.Background(), &lobslawv1.SubmitEnrolmentRequest{
		RequestedName: "alice", CsrDer: []byte("not a csr"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", status.Code(err))
	}
}

// --- deciding requires an identified operator --------------------------

// THE CENTRAL REFUSAL. An unattributed approval is an operator
// credential nobody is accountable for.
func TestDecidingWithoutAVerifiedIdentityIsRefused(t *testing.T) {
	t.Parallel()
	svc := newEnrolmentSvc(t)
	req := submitTo(t, svc, "alice")

	// No peer info on the context: exactly what an unauthenticated
	// caller reaching this RPC would present.
	_, err := svc.DecideEnrolment(context.Background(), &lobslawv1.DecideEnrolmentRequest{
		Id: req.GetId(), Approve: true, Fingerprint: req.GetFingerprint(),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", status.Code(err))
	}

	// And nothing was issued.
	poll, perr := svc.PollEnrolment(context.Background(),
		&lobslawv1.PollEnrolmentRequest{Id: req.GetId()})
	if perr != nil {
		t.Fatal(perr)
	}
	if poll.GetState() != lobslawv1.EnrolmentState_ENROLMENT_STATE_PENDING {
		t.Errorf("state = %s; an unattributed call decided it", poll.GetState())
	}
}

// --- the fingerprint pin -----------------------------------------------

// A caller who verified a fingerprint out of band pins it. A request
// that changed underneath them is refused rather than approved in
// place of the one they checked.
func TestAPinnedFingerprintThatDoesNotMatchIsRefused(t *testing.T) {
	t.Parallel()
	svc := newEnrolmentSvc(t)
	req := submitTo(t, svc, "alice")

	_, err := svc.DecideEnrolment(operatorCtx(t, "user:owner"), &lobslawv1.DecideEnrolmentRequest{
		Id: req.GetId(), Approve: true, Fingerprint: "SHA256:not:the:one",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
	if err != nil && !containsAll(err.Error(), "fingerprint", req.GetFingerprint()) {
		t.Errorf("error %q does not show both fingerprints", err)
	}
}

// --- issuing -----------------------------------------------------------

func TestAnApprovedRequestYieldsACollectableCertificate(t *testing.T) {
	t.Parallel()
	svc := newEnrolmentSvc(t)
	req := submitTo(t, svc, "alice")

	if _, err := svc.DecideEnrolment(operatorCtx(t, "user:owner"), &lobslawv1.DecideEnrolmentRequest{
		Id: req.GetId(), Approve: true, Fingerprint: req.GetFingerprint(),
	}); err != nil {
		t.Fatal(err)
	}

	poll, err := svc.PollEnrolment(context.Background(),
		&lobslawv1.PollEnrolmentRequest{Id: req.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if poll.GetState() != lobslawv1.EnrolmentState_ENROLMENT_STATE_ISSUED {
		t.Fatalf("state = %s", poll.GetState())
	}
	// All three, because a certificate alone is not usable: the
	// operator root builds the chain to present, the cluster root
	// verifies the node on every later connection.
	if len(poll.GetCertPem()) == 0 {
		t.Error("no certificate returned")
	}
	if len(poll.GetCaPem()) == 0 {
		t.Error("no operator root returned; the laptop cannot present a chain")
	}
	if len(poll.GetClusterCaPem()) == 0 {
		t.Error("no cluster root returned; the laptop cannot verify the node")
	}
}

// A pending request must not leak roots — there is nothing to build a
// chain with yet, and sending them implies there is.
func TestAPendingPollReturnsNoMaterial(t *testing.T) {
	t.Parallel()
	svc := newEnrolmentSvc(t)
	req := submitTo(t, svc, "alice")

	poll, err := svc.PollEnrolment(context.Background(),
		&lobslawv1.PollEnrolmentRequest{Id: req.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if len(poll.GetCertPem()) != 0 || len(poll.GetCaPem()) != 0 || len(poll.GetClusterCaPem()) != 0 {
		t.Error("a pending request returned certificate material")
	}
}

func TestPollingAnUnknownRequestIsNotFound(t *testing.T) {
	t.Parallel()
	svc := newEnrolmentSvc(t)
	_, err := svc.PollEnrolment(context.Background(), &lobslawv1.PollEnrolmentRequest{Id: "ghost"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("code = %v, want NotFound", status.Code(err))
	}
}

// An approval that cannot issue is not an approval, and recording one
// would leave a laptop polling for a certificate nothing will produce.
func TestApprovingWithNoOperatorCAIsRefused(t *testing.T) {
	t.Parallel()
	svc := newEnrolmentSvc(t)
	req := submitTo(t, svc, "alice")
	svc.caCert, svc.caKey = nil, nil

	_, err := svc.DecideEnrolment(operatorCtx(t, "user:owner"), &lobslawv1.DecideEnrolmentRequest{
		Id: req.GetId(), Approve: true, Fingerprint: req.GetFingerprint(),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", status.Code(err))
	}
}

// --- listing -----------------------------------------------------------

// The CSR is dropped from a listing: public, but several hundred bytes
// of noise per row, and the fingerprint is what a human compares.
func TestTheListingOmitsTheRequestBody(t *testing.T) {
	t.Parallel()
	svc := newEnrolmentSvc(t)
	submitTo(t, svc, "alice")

	res, err := svc.ListEnrolments(context.Background(), &lobslawv1.ListEnrolmentsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetEnrolments()) != 1 {
		t.Fatalf("got %d", len(res.GetEnrolments()))
	}
	if len(res.GetEnrolments()[0].GetCsrDer()) != 0 {
		t.Error("the listing carries the full certificate request")
	}
	if res.GetEnrolments()[0].GetFingerprint() == "" {
		t.Error("the listing carries no fingerprint, which is what an approver compares")
	}
}

func containsAll(s string, want ...string) bool {
	for _, w := range want {
		if !strings.Contains(s, w) {
			return false
		}
	}
	return true
}
