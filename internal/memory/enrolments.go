package memory

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Pending operator enrolments.
//
// Replicated rather than in-memory, unlike the confirmation registry.
// A request that vanished on restart would leave a laptop polling an
// id no node has heard of, with no way to tell that from "still
// waiting" — and it lets the node that ANSWERS an enrolment be a
// different one from the node that received it.

var (
	// ErrEnrolmentNotFound is returned for an unknown id.
	ErrEnrolmentNotFound = errors.New("enrolment: no such request")
	// ErrEnrolmentDecided means somebody answered first. Expected when
	// two approvers race, not an error worth alarming about.
	ErrEnrolmentDecided = errors.New("enrolment: already decided")
)

// DefaultEnrolmentTTL bounds how long a request waits for an answer.
//
// Short on purpose. An operator returning to a week-old request has no
// idea whether the laptop that asked is still the one they would be
// admitting, and approving it anyway is how a stale request becomes a
// live credential.
const DefaultEnrolmentTTL = 30 * time.Minute

// EnrolmentStore is the Raft-backed request registry.
type EnrolmentStore struct {
	raft  raftApplier
	store *Store
	log   *slog.Logger
	ttl   time.Duration
}

type EnrolmentStoreConfig struct {
	Raft  raftApplier
	Store *Store
	TTL   time.Duration
	Log   *slog.Logger
}

func NewEnrolmentStore(cfg EnrolmentStoreConfig) (*EnrolmentStore, error) {
	if cfg.Raft == nil || cfg.Store == nil {
		return nil, errors.New("enrolment store: Raft and Store are both required")
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = DefaultEnrolmentTTL
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &EnrolmentStore{raft: cfg.Raft, store: cfg.Store, log: log, ttl: ttl}, nil
}

// Fingerprint renders a public key for a human to compare.
//
// SHA-256 of the DER-encoded public key, colon-separated hex — the
// shape people already read off SSH and TLS tooling. This string is
// the only defence against approving somebody else's request that
// arrived at the same moment, so it has to be short enough that a
// person actually checks it rather than glancing at the first four
// characters.
func Fingerprint(pub any) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("marshal public key: %w", err)
	}
	sum := sha256.Sum256(der)
	out := make([]string, 0, len(sum))
	for _, b := range sum {
		out = append(out, hex.EncodeToString([]byte{b}))
	}
	return "SHA256:" + strings.Join(out, ":"), nil
}

// Submit records a new request and returns it.
//
// The CSR is parsed HERE rather than trusted: a request that does not
// decode, or that does not prove possession of its own key, is refused
// before it can occupy a slot in somebody's approval queue.
func (e *EnrolmentStore) Submit(requestedName string, csrDER []byte) (*lobslawv1.EnrolmentRecord, error) {
	name := strings.TrimSpace(requestedName)
	if name == "" {
		return nil, errors.New("enrolment: a requested name is required")
	}
	if len(csrDER) == 0 {
		return nil, errors.New("enrolment: a certificate request is required")
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("enrolment: parse request: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		// Proof of possession, checked at submission as well as at
		// signing. Without it anyone could queue a request for a key
		// they do not hold, and the approver would be deciding about
		// somebody else's laptop.
		return nil, fmt.Errorf("enrolment: request does not prove key possession: %w", err)
	}
	fp, err := Fingerprint(csr.PublicKey)
	if err != nil {
		return nil, err
	}

	id, err := randomEnrolmentID()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	rec := &lobslawv1.EnrolmentRecord{
		Id:            id,
		RequestedName: name,
		CsrDer:        csrDER,
		Fingerprint:   fp,
		State:         lobslawv1.EnrolmentState_ENROLMENT_STATE_PENDING,
		CreatedAt:     timestamppb.New(now),
		ExpiresAt:     timestamppb.New(now.Add(e.ttl)),
	}
	if err := e.put(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// Get reads one request.
func (e *EnrolmentStore) Get(id string) (*lobslawv1.EnrolmentRecord, error) {
	raw, err := e.store.Get(BucketEnrolments, id)
	if err != nil {
		if IsNotFound(err) {
			return nil, ErrEnrolmentNotFound
		}
		return nil, err
	}
	var rec lobslawv1.EnrolmentRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("enrolment: decode %s: %w", id, err)
	}
	return &rec, nil
}

// List returns every request, newest first.
func (e *EnrolmentStore) List(pendingOnly bool) ([]*lobslawv1.EnrolmentRecord, error) {
	var out []*lobslawv1.EnrolmentRecord
	err := e.store.ForEach(BucketEnrolments, func(key string, raw []byte) error {
		var rec lobslawv1.EnrolmentRecord
		if uerr := proto.Unmarshal(raw, &rec); uerr != nil {
			return fmt.Errorf("enrolment: decode %s: %w", key, uerr)
		}
		if pendingOnly && rec.GetState() != lobslawv1.EnrolmentState_ENROLMENT_STATE_PENDING {
			return nil
		}
		out = append(out, &rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortByCreatedDesc(out)
	return out, nil
}

// Decide records an approval or denial.
//
// signer produces the certificate; it is called only on approval and
// only after the compare-and-set has been prepared, so a losing racer
// never signs anything. Passing it in keeps the CA out of this
// package — the store knows about requests, not about keys.
func (e *EnrolmentStore) Decide(id string, approve bool, issuedName, by string,
	signer func(csr *x509.CertificateRequest, name string) ([]byte, error)) (
	*lobslawv1.EnrolmentRecord, error) {
	current, err := e.Get(id)
	if err != nil {
		return nil, err
	}
	if current.GetState() != lobslawv1.EnrolmentState_ENROLMENT_STATE_PENDING {
		return nil, ErrEnrolmentDecided
	}
	if enrolmentExpired(current, time.Now()) {
		// Refused rather than approved-anyway. The sweeper may not have
		// run yet, and "it was still in the list" is not a reason to
		// admit a request whose window closed.
		return nil, fmt.Errorf("enrolment: %s expired at %s",
			id, current.GetExpiresAt().AsTime().Format(time.RFC3339))
	}

	updated := proto.Clone(current).(*lobslawv1.EnrolmentRecord)
	updated.DecidedBy = by
	updated.DecidedAt = timestamppb.New(time.Now())
	updated.ClaimedBy = by

	if !approve {
		updated.State = lobslawv1.EnrolmentState_ENROLMENT_STATE_DENIED
		return e.claim(current, updated)
	}

	name := strings.TrimSpace(issuedName)
	if name == "" {
		name = current.GetRequestedName()
	}
	csr, err := x509.ParseCertificateRequest(current.GetCsrDer())
	if err != nil {
		return nil, fmt.Errorf("enrolment: parse stored request: %w", err)
	}
	certPEM, err := signer(csr, name)
	if err != nil {
		return nil, fmt.Errorf("enrolment: sign: %w", err)
	}
	updated.State = lobslawv1.EnrolmentState_ENROLMENT_STATE_ISSUED
	updated.IssuedName = name
	updated.CertPem = certPEM
	return e.claim(current, updated)
}

// Sweep expires every request past its window and returns how many it
// closed.
//
// A sweeper rather than a timer, for the reason the prompt store gives:
// a timer is per-process, so a request created on a node that then
// dies would stay approvable forever.
func (e *EnrolmentStore) Sweep(now time.Time) (int, error) {
	pending, err := e.List(true)
	if err != nil {
		return 0, err
	}
	var closed int
	for _, rec := range pending {
		if !enrolmentExpired(rec, now) {
			continue
		}
		updated := proto.Clone(rec).(*lobslawv1.EnrolmentRecord)
		updated.State = lobslawv1.EnrolmentState_ENROLMENT_STATE_EXPIRED
		updated.ClaimedBy = "sweeper"
		if _, cerr := e.claim(rec, updated); cerr != nil {
			if errors.Is(cerr, ErrClaimConflict) {
				// Somebody answered it between the list and the write.
				// Their answer wins.
				continue
			}
			return closed, cerr
		}
		closed++
	}
	return closed, nil
}

// claim applies the compare-and-set.
func (e *EnrolmentStore) claim(current, updated *lobslawv1.EnrolmentRecord) (
	*lobslawv1.EnrolmentRecord, error) {
	rev := current.GetRevision()
	entry := &lobslawv1.LogEntry{
		Op:               lobslawv1.LogOp_LOG_OP_CLAIM,
		Id:               current.GetId(),
		Payload:          &lobslawv1.LogEntry_Enrolment{Enrolment: updated},
		ExpectedClaimer:  current.GetClaimedBy(),
		ExpectedRevision: &rev,
	}
	if err := e.apply(entry); err != nil {
		// Only a refused CAS means somebody decided first. Collapsing
		// every failure into that would report "already decided" when
		// the truth was a raft timeout.
		if errors.Is(err, ErrClaimConflict) {
			return nil, ErrEnrolmentDecided
		}
		return nil, err
	}
	return updated, nil
}

func (e *EnrolmentStore) put(rec *lobslawv1.EnrolmentRecord) error {
	return e.apply(&lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      rec.GetId(),
		Payload: &lobslawv1.LogEntry_Enrolment{Enrolment: rec},
	})
}

func (e *EnrolmentStore) apply(entry *lobslawv1.LogEntry) error {
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("enrolment: marshal: %w", err)
	}
	res, err := e.raft.Apply(data, 5*time.Second)
	if err != nil {
		return fmt.Errorf("enrolment: raft apply: %w", err)
	}
	if ferr, ok := res.(error); ok && ferr != nil {
		return ferr
	}
	return nil
}

func enrolmentExpired(rec *lobslawv1.EnrolmentRecord, now time.Time) bool {
	exp := rec.GetExpiresAt()
	if exp == nil {
		return false
	}
	return now.After(exp.AsTime())
}

func sortByCreatedDesc(recs []*lobslawv1.EnrolmentRecord) {
	for i := 1; i < len(recs); i++ {
		for j := i; j > 0 && LaterThan(recs[j].GetCreatedAt(), recs[j-1].GetCreatedAt()); j-- {
			recs[j], recs[j-1] = recs[j-1], recs[j]
		}
	}
}

// randomEnrolmentID returns 32 hex chars.
//
// Unguessable, which is what lets Poll take the id alone as proof of
// being the submitter. That is capability-by-obscurity, and it is
// acceptable HERE for a reason it was not acceptable for prompt
// callbacks: what a guesser would obtain is a CERTIFICATE, which is
// public and useless without the private key it certifies. Approving
// somebody else's confirmation changes the world; reading somebody
// else's certificate does not.
func randomEnrolmentID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("enrolment: generate id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
