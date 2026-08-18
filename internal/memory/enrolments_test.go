package memory

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"

	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// The point of enrolment is that the private key never moves: the
// laptop generates it, keeps the private half, and sends a request.
// These tests hold the store to the two things that make that safe —
// only one approver can issue, and a request nobody answers closes.

func newEnrolmentStore(t *testing.T) *EnrolmentStore {
	t.Helper()
	svc := newTestServiceStack(t)
	es, err := NewEnrolmentStore(EnrolmentStoreConfig{Raft: svc.raft, Store: svc.store})
	if err != nil {
		t.Fatal(err)
	}
	return es
}

// csrFor builds a request the way an enrolling laptop would.
func csrFor(t *testing.T, subject string) ([]byte, ed25519.PrivateKey) {
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
	return der, priv
}

// stubSigner stands in for the operator CA. Records what it was asked
// to certify, so a test can check the NAME reaching it.
type stubSigner struct {
	sawName string
	calls   int
	err     error
}

func (s *stubSigner) sign(_ *x509.CertificateRequest, name string) ([]byte, error) {
	s.calls++
	s.sawName = name
	if s.err != nil {
		return nil, s.err
	}
	return []byte("-----BEGIN CERTIFICATE-----\nstub\n-----END CERTIFICATE-----\n"), nil
}

func submit(t *testing.T, es *EnrolmentStore, name string) *lobslawv1.EnrolmentRecord {
	t.Helper()
	der, _ := csrFor(t, name)
	rec, err := es.Submit(name, der)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// --- submitting --------------------------------------------------------

func TestASubmittedRequestIsPendingAndFingerprinted(t *testing.T) {
	t.Parallel()
	es := newEnrolmentStore(t)

	rec := submit(t, es, "alice")
	if rec.GetState() != lobslawv1.EnrolmentState_ENROLMENT_STATE_PENDING {
		t.Errorf("state = %s", rec.GetState())
	}
	if !strings.HasPrefix(rec.GetFingerprint(), "SHA256:") {
		t.Errorf("fingerprint = %q; an approver has nothing to compare", rec.GetFingerprint())
	}
	if rec.GetExpiresAt() == nil {
		t.Error("a request with no expiry stays approvable forever")
	}
}

// Proof of possession, checked at SUBMISSION as well as at signing.
// Without it anyone could queue a request for a key they do not hold,
// and the approver would be deciding about somebody else's laptop.
func TestARequestThatDoesNotProvePossessionIsRefused(t *testing.T) {
	t.Parallel()
	es := newEnrolmentStore(t)
	der, _ := csrFor(t, "alice")
	der[len(der)-1] ^= 0xff

	if _, err := es.Submit("alice", der); err == nil {
		t.Fatal("a request that does not prove key possession was queued")
	}
}

func TestSubmissionNeedsANameAndARequest(t *testing.T) {
	t.Parallel()
	es := newEnrolmentStore(t)
	der, _ := csrFor(t, "alice")

	if _, err := es.Submit("", der); err == nil {
		t.Error("an unnamed request was queued")
	}
	if _, err := es.Submit("alice", nil); err == nil {
		t.Error("a request with no CSR was queued")
	}
	if _, err := es.Submit("alice", []byte("not der")); err == nil {
		t.Error("an undecodable request was queued")
	}
}

// Two laptops enrolling at once must get different fingerprints, or
// the approver's only check is worthless.
func TestTwoRequestsHaveDifferentFingerprints(t *testing.T) {
	t.Parallel()
	es := newEnrolmentStore(t)

	a, b := submit(t, es, "alice"), submit(t, es, "alice")
	if a.GetFingerprint() == b.GetFingerprint() {
		t.Error("two distinct keys produced the same fingerprint")
	}
	if a.GetId() == b.GetId() {
		t.Error("two requests share an id")
	}
}

// --- deciding ----------------------------------------------------------

func TestApprovalIssuesACertificate(t *testing.T) {
	t.Parallel()
	es := newEnrolmentStore(t)
	rec := submit(t, es, "alice")
	signer := &stubSigner{}

	out, err := es.Decide(rec.GetId(), true, "", "user:owner", signer.sign)
	if err != nil {
		t.Fatal(err)
	}
	if out.GetState() != lobslawv1.EnrolmentState_ENROLMENT_STATE_ISSUED {
		t.Errorf("state = %s", out.GetState())
	}
	if len(out.GetCertPem()) == 0 {
		t.Error("an approved request carries no certificate")
	}
	if out.GetDecidedBy() != "user:owner" {
		t.Errorf("decided_by = %q; the audit trail does not say who", out.GetDecidedBy())
	}
}

// The approver may rename. The requested name is what the laptop
// typed; the issued one is what somebody agreed to.
func TestTheApproverCanOverrideTheRequestedName(t *testing.T) {
	t.Parallel()
	es := newEnrolmentStore(t)
	rec := submit(t, es, "root")
	signer := &stubSigner{}

	out, err := es.Decide(rec.GetId(), true, "alice", "user:owner", signer.sign)
	if err != nil {
		t.Fatal(err)
	}
	if signer.sawName != "alice" {
		t.Errorf("signed as %q; the approver's choice did not reach the CA", signer.sawName)
	}
	if out.GetIssuedName() != "alice" || out.GetRequestedName() != "root" {
		t.Errorf("issued=%q requested=%q; the difference is not visible",
			out.GetIssuedName(), out.GetRequestedName())
	}
}

func TestAnEmptyNameAcceptsWhatWasRequested(t *testing.T) {
	t.Parallel()
	es := newEnrolmentStore(t)
	rec := submit(t, es, "alice")
	signer := &stubSigner{}

	if _, err := es.Decide(rec.GetId(), true, "", "user:owner", signer.sign); err != nil {
		t.Fatal(err)
	}
	if signer.sawName != "alice" {
		t.Errorf("signed as %q, want the requested name", signer.sawName)
	}
}

// Denial must not sign anything.
func TestDenialIssuesNothing(t *testing.T) {
	t.Parallel()
	es := newEnrolmentStore(t)
	rec := submit(t, es, "alice")
	signer := &stubSigner{}

	out, err := es.Decide(rec.GetId(), false, "", "user:owner", signer.sign)
	if err != nil {
		t.Fatal(err)
	}
	if out.GetState() != lobslawv1.EnrolmentState_ENROLMENT_STATE_DENIED {
		t.Errorf("state = %s", out.GetState())
	}
	if signer.calls != 0 {
		t.Error("a denial called the CA")
	}
	if len(out.GetCertPem()) != 0 {
		t.Error("a denied request carries a certificate")
	}
}

// THE RACE. Two approvers answering at once must not both issue — the
// second read is stale and its write is refused.
func TestOnlyOneApproverCanIssue(t *testing.T) {
	t.Parallel()
	es := newEnrolmentStore(t)
	rec := submit(t, es, "alice")
	first, second := &stubSigner{}, &stubSigner{}

	if _, err := es.Decide(rec.GetId(), true, "", "user:a", first.sign); err != nil {
		t.Fatal(err)
	}
	_, err := es.Decide(rec.GetId(), true, "", "user:b", second.sign)
	if !errors.Is(err, ErrEnrolmentDecided) {
		t.Fatalf("second approval returned %v, want ErrEnrolmentDecided", err)
	}
	if second.calls != 0 {
		t.Error("the losing approver still signed a certificate")
	}
}

// THE ACTUAL RACE, as opposed to the sequential case above.
//
// TestOnlyOneApproverCanIssue is caught by the state check, which is a
// read-then-write with a gap in the middle: on a real cluster two
// nodes can both read PENDING and both proceed. What stops the second
// write is the compare-and-set, and reaching it needs a STALE read —
// exactly what a losing racer holds.
func TestALosingApproverIsRefusedByTheCompareAndSet(t *testing.T) {
	t.Parallel()
	es := newEnrolmentStore(t)
	rec := submit(t, es, "alice")

	// Both approvers read the pending record.
	stale, err := es.Get(rec.GetId())
	if err != nil {
		t.Fatal(err)
	}

	// The first one lands.
	if _, derr := es.Decide(rec.GetId(), true, "", "user:a", (&stubSigner{}).sign); derr != nil {
		t.Fatal(derr)
	}

	// The second writes from its stale read, having passed the state
	// check before the first one committed.
	updated := proto.Clone(stale).(*lobslawv1.EnrolmentRecord)
	updated.State = lobslawv1.EnrolmentState_ENROLMENT_STATE_DENIED
	updated.DecidedBy = "user:b"
	updated.ClaimedBy = "user:b"

	if _, cerr := es.claim(stale, updated); !errors.Is(cerr, ErrEnrolmentDecided) {
		t.Fatalf("a stale write returned %v, want ErrEnrolmentDecided", cerr)
	}

	// And the first answer survived intact.
	after, err := es.Get(rec.GetId())
	if err != nil {
		t.Fatal(err)
	}
	if after.GetState() != lobslawv1.EnrolmentState_ENROLMENT_STATE_ISSUED {
		t.Errorf("state = %s; the loser overwrote the winner", after.GetState())
	}
	if after.GetDecidedBy() != "user:a" {
		t.Errorf("decided_by = %q; the audit trail names the wrong person", after.GetDecidedBy())
	}
}

// A denial closes it too — approving after a denial would quietly
// overturn somebody's refusal.
func TestApprovingAfterADenialIsRefused(t *testing.T) {
	t.Parallel()
	es := newEnrolmentStore(t)
	rec := submit(t, es, "alice")
	signer := &stubSigner{}

	if _, err := es.Decide(rec.GetId(), false, "", "user:a", signer.sign); err != nil {
		t.Fatal(err)
	}
	if _, err := es.Decide(rec.GetId(), true, "", "user:b", signer.sign); !errors.Is(err, ErrEnrolmentDecided) {
		t.Fatalf("approving a denied request returned %v", err)
	}
	if signer.calls != 0 {
		t.Error("a certificate was issued for a denied request")
	}
}

// A signing failure must not mark the request issued. Recording
// success for a certificate that does not exist would leave a laptop
// polling forever with nothing to collect.
func TestASigningFailureLeavesTheRequestPending(t *testing.T) {
	t.Parallel()
	es := newEnrolmentStore(t)
	rec := submit(t, es, "alice")
	signer := &stubSigner{err: errors.New("CA unavailable")}

	if _, err := es.Decide(rec.GetId(), true, "", "user:owner", signer.sign); err == nil {
		t.Fatal("a failed signing was reported as an approval")
	}
	after, err := es.Get(rec.GetId())
	if err != nil {
		t.Fatal(err)
	}
	if after.GetState() != lobslawv1.EnrolmentState_ENROLMENT_STATE_PENDING {
		t.Errorf("state = %s; a failed signing consumed the request", after.GetState())
	}
}

func TestDecidingAnUnknownRequestIsNotFound(t *testing.T) {
	t.Parallel()
	es := newEnrolmentStore(t)
	if _, err := es.Decide("ghost", true, "", "user:owner", (&stubSigner{}).sign); !errors.Is(err, ErrEnrolmentNotFound) {
		t.Errorf("got %v, want ErrEnrolmentNotFound", err)
	}
}

// --- expiry ------------------------------------------------------------

// An operator returning to a stale request has no idea whether the
// laptop that asked is still the one they would be admitting.
func TestAnExpiredRequestCannotBeApproved(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	es, err := NewEnrolmentStore(EnrolmentStoreConfig{
		Raft: svc.raft, Store: svc.store, TTL: time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := submit(t, es, "alice")
	time.Sleep(2 * time.Millisecond)
	signer := &stubSigner{}

	if _, derr := es.Decide(rec.GetId(), true, "", "user:owner", signer.sign); derr == nil {
		t.Fatal("an expired request was approved")
	}
	if signer.calls != 0 {
		t.Error("an expired request was signed")
	}
}

// The sweeper closes what nobody answered. A timer would not: it is
// per-process, so a request created on a node that then dies would
// stay approvable forever.
func TestTheSweeperClosesStaleRequests(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	es, err := NewEnrolmentStore(EnrolmentStoreConfig{
		Raft: svc.raft, Store: svc.store, TTL: time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := submit(t, es, "alice")
	time.Sleep(2 * time.Millisecond)

	closed, err := es.Sweep(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Fatalf("swept %d requests, want 1", closed)
	}
	after, err := es.Get(rec.GetId())
	if err != nil {
		t.Fatal(err)
	}
	if after.GetState() != lobslawv1.EnrolmentState_ENROLMENT_STATE_EXPIRED {
		t.Errorf("state = %s", after.GetState())
	}
}

// A live request must survive the sweeper, or enrolment never works.
func TestTheSweeperLeavesLiveRequestsAlone(t *testing.T) {
	t.Parallel()
	es := newEnrolmentStore(t)
	rec := submit(t, es, "alice")

	closed, err := es.Sweep(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if closed != 0 {
		t.Errorf("swept %d live requests", closed)
	}
	after, _ := es.Get(rec.GetId())
	if after.GetState() != lobslawv1.EnrolmentState_ENROLMENT_STATE_PENDING {
		t.Errorf("state = %s", after.GetState())
	}
}

// --- listing -----------------------------------------------------------

func TestPendingOnlyNarrowsTheList(t *testing.T) {
	t.Parallel()
	es := newEnrolmentStore(t)
	open1 := submit(t, es, "alice")
	closed := submit(t, es, "bob")
	if _, err := es.Decide(closed.GetId(), false, "", "user:owner", (&stubSigner{}).sign); err != nil {
		t.Fatal(err)
	}

	all, err := es.List(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("unfiltered list = %d, want 2", len(all))
	}
	pending, err := es.List(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].GetId() != open1.GetId() {
		t.Errorf("pending list = %v", pending)
	}
}

// --- the fingerprint ---------------------------------------------------

// The approver compares this string against what the laptop printed.
// It has to be the shape people already read off SSH and TLS tooling,
// or they will glance at the first four characters and move on.
func TestTheFingerprintIsStableAndReadable(t *testing.T) {
	t.Parallel()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Fingerprint(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Fingerprint(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("the same key fingerprinted differently twice")
	}
	if !strings.HasPrefix(first, "SHA256:") || strings.Count(first, ":") != 32 {
		t.Errorf("fingerprint = %q; not the familiar colon-separated shape", first)
	}
}
