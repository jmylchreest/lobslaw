package node

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"errors"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"log/slog"

	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/grpcinterceptors"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Issuing an operator credential to a key the requester already holds.
//
// The store knows about requests; the operator CA knows about keys.
// This is the seam between them, and it is the only place both are in
// scope — which is why the signing-key handling lives here rather than
// in internal/memory.

// EnrolmentAsker puts a pending request in front of a human over a
// channel.
//
// An interface so the node does not reach into a specific gateway, and
// so a deployment with no channel wired simply asks nobody — which is
// not a failure, because an operator can still decide from the CLI.
type EnrolmentAsker interface {
	AskEnrolment(ctx context.Context, req gateway.EnrolmentRequest) (promptID string, err error)
}

// enrolmentService serves EnrolmentService.
type enrolmentService struct {
	lobslawv1.UnimplementedEnrolmentServiceServer

	asker      EnrolmentAsker
	log        *slog.Logger
	store      *memory.EnrolmentStore
	caCert     *x509.Certificate
	caKey      ed25519.PrivateKey
	clusterCA  []byte
	operatorCA []byte
	validFor   time.Duration
}

// SubmitEnrolment queues a request. Served WITHOUT a client
// certificate: a laptop enrolling does not have one yet.
func (s *enrolmentService) SubmitEnrolment(ctx context.Context, req *lobslawv1.SubmitEnrolmentRequest) (
	*lobslawv1.SubmitEnrolmentResponse, error) {
	if s.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "enrolment is not configured on this node")
	}
	rec, err := s.store.Submit(req.GetRequestedName(), req.GetCsrDer())
	if err != nil {
		// The caller's mistake in every case Submit rejects: an empty
		// name, a request that does not decode, or one that does not
		// prove possession of its own key.
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	// Asked AFTER the request is durably queued, and never fatally.
	// A channel outage must not lose an enrolment somebody could still
	// approve from the CLI — the request is the record, the prompt is
	// only a way of noticing it.
	s.ask(ctx, rec)

	return &lobslawv1.SubmitEnrolmentResponse{
		Id:          rec.GetId(),
		Fingerprint: rec.GetFingerprint(),
		ExpiresAt:   rec.GetExpiresAt(),
	}, nil
}

// ask raises the approval prompt, if a channel is wired.
func (s *enrolmentService) ask(ctx context.Context, rec *lobslawv1.EnrolmentRecord) {
	if s.asker == nil {
		return
	}
	var expires time.Time
	if e := rec.GetExpiresAt(); e != nil {
		expires = e.AsTime()
	}
	promptID, err := s.asker.AskEnrolment(ctx, gateway.EnrolmentRequest{
		ID:            rec.GetId(),
		RequestedName: rec.GetRequestedName(),
		Fingerprint:   rec.GetFingerprint(),
		ExpiresAt:     expires,
	})
	if err != nil {
		// Logged at WARN, not returned. Nobody was asked, which is
		// worth knowing, but the request is queued and decidable.
		s.log.Warn("enrolment: nobody could be asked to approve",
			"enrolment", rec.GetId(), "err", err)
		return
	}
	s.log.Info("enrolment: approval requested",
		"enrolment", rec.GetId(), "prompt", promptID)
}

// DecideFromChannel applies a decision reached over a channel.
//
// Satisfies gateway.EnrolmentDecider. Separate from DecideEnrolment
// because the identity arrives differently: over gRPC it comes from a
// verified client certificate, and here it comes from the prompt's
// audience check, which the gateway has already performed.
func (s *enrolmentService) Decide(ctx context.Context, id string, approve bool, by string) error {
	if s.store == nil {
		return errors.New("enrolment is not configured on this node")
	}
	if strings.TrimSpace(by) == "" {
		// Same rule as the gRPC path. An unattributed approval is an
		// operator credential nobody is accountable for.
		return errors.New("an enrolment decision must name who made it")
	}
	if approve && (s.caCert == nil || s.caKey == nil) {
		return errors.New("this node holds no operator CA and cannot issue certificates")
	}
	_, err := s.store.Decide(id, approve, "", by,
		func(csr *x509.CertificateRequest, name string) ([]byte, error) {
			return mtls.SignOperatorCSR(s.caCert, s.caKey, csr, name, s.validFor)
		})
	_ = ctx
	return err
}

// PollEnrolment reports whether a request has been answered.
func (s *enrolmentService) PollEnrolment(_ context.Context, req *lobslawv1.PollEnrolmentRequest) (
	*lobslawv1.PollEnrolmentResponse, error) {
	if s.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "enrolment is not configured on this node")
	}
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	rec, err := s.store.Get(req.GetId())
	if err != nil {
		if errors.Is(err, memory.ErrEnrolmentNotFound) {
			return nil, status.Errorf(codes.NotFound, "no enrolment request %q", req.GetId())
		}
		return nil, status.Errorf(codes.Internal, "read enrolment: %v", err)
	}

	out := &lobslawv1.PollEnrolmentResponse{
		State:      rec.GetState(),
		IssuedName: rec.GetIssuedName(),
	}
	if rec.GetState() == lobslawv1.EnrolmentState_ENROLMENT_STATE_ISSUED {
		// The roots go back WITH the certificate rather than being
		// fetched separately. A laptop that had the cert but not the
		// operator root could not build a chain to present, and one
		// without the cluster root could not verify the node it is
		// about to trust.
		out.CertPem = rec.GetCertPem()
		out.CaPem = s.operatorCA
		out.ClusterCaPem = s.clusterCA
	}
	return out, nil
}

// ListEnrolments shows what is waiting. Requires an operator
// credential — it is served on the cluster listener.
func (s *enrolmentService) ListEnrolments(_ context.Context, req *lobslawv1.ListEnrolmentsRequest) (
	*lobslawv1.ListEnrolmentsResponse, error) {
	if s.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "enrolment is not configured on this node")
	}
	recs, err := s.store.List(req.GetPendingOnly())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list enrolments: %v", err)
	}
	// The CSR is dropped from a listing. It is public, but it is also
	// several hundred bytes of noise per row, and nothing reading a
	// list needs it — the fingerprint is what a human compares.
	for _, r := range recs {
		r.CsrDer = nil
	}
	return &lobslawv1.ListEnrolmentsResponse{Enrolments: recs}, nil
}

// DecideEnrolment approves or denies.
func (s *enrolmentService) DecideEnrolment(ctx context.Context, req *lobslawv1.DecideEnrolmentRequest) (
	*lobslawv1.DecideEnrolmentResponse, error) {
	if s.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "enrolment is not configured on this node")
	}
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if req.GetApprove() && (s.caCert == nil || s.caKey == nil) {
		// Refused rather than recorded-and-deferred. An approval that
		// cannot issue is not an approval, and marking it so would
		// leave a laptop polling for a certificate nothing will ever
		// produce.
		return nil, status.Error(codes.FailedPrecondition,
			"this node holds no operator CA and cannot issue certificates")
	}

	by := operatorFromContext(ctx)
	if by == "" {
		// Every decision names somebody. An unattributed approval is
		// an operator credential nobody is accountable for.
		return nil, status.Error(codes.PermissionDenied,
			"an enrolment decision must be made by an identified operator")
	}

	if want := strings.TrimSpace(req.GetFingerprint()); want != "" {
		current, err := s.store.Get(req.GetId())
		if err != nil {
			return nil, status.Errorf(codes.NotFound, "no enrolment request %q", req.GetId())
		}
		if current.GetFingerprint() != want {
			// The caller verified a fingerprint out of band and pinned
			// it. A request that changed underneath them is refused
			// rather than approved in place of the one they checked.
			return nil, status.Errorf(codes.FailedPrecondition,
				"fingerprint mismatch: request carries %s, you pinned %s",
				current.GetFingerprint(), want)
		}
	}

	validFor := s.validFor
	if d := req.GetValidFor(); d != nil && d.AsDuration() > 0 {
		validFor = d.AsDuration()
	}

	rec, err := s.store.Decide(req.GetId(), req.GetApprove(), req.GetName(), by,
		func(csr *x509.CertificateRequest, name string) ([]byte, error) {
			return mtls.SignOperatorCSR(s.caCert, s.caKey, csr, name, validFor)
		})
	switch {
	case errors.Is(err, memory.ErrEnrolmentNotFound):
		return nil, status.Errorf(codes.NotFound, "no enrolment request %q", req.GetId())
	case errors.Is(err, memory.ErrEnrolmentDecided):
		return nil, status.Errorf(codes.FailedPrecondition, "enrolment %q was already decided", req.GetId())
	case err != nil:
		return nil, status.Errorf(codes.Internal, "decide enrolment: %v", err)
	}
	return &lobslawv1.DecideEnrolmentResponse{Enrolment: rec}, nil
}

// operatorFromContext names the caller from their verified client
// certificate.
//
// Read from the VERIFIED chain rather than from anything the request
// carries. A name in the request body is chosen by the sender; a
// CommonName in a chain the server verified is not.
func operatorFromContext(ctx context.Context) string {
	cert := grpcinterceptors.VerifiedPeerCert(ctx)
	if cert == nil {
		return ""
	}
	return cert.Subject.CommonName
}
