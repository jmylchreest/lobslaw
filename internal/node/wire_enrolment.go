package node

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/singleton"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Enrolling an operator whose laptop has no credential yet.
//
// Two surfaces, deliberately apart:
//
//   - Submit and Poll are served on their OWN listener, without a
//     client certificate, because the caller does not have one. That
//     is the entire point of enrolment.
//   - List and Decide ride the cluster listener with everything else
//     and require an operator credential.
//
// Separate listeners rather than one with a relaxed policy. The
// cluster listener's guarantee is that every caller presents a
// verified certificate; weakening it to let enrolment through would
// trade that guarantee for a convenience, and the guarantee is what
// the whole trust model rests on.

// operatorCAPaths resolves where the operator root lives, defaulting
// beside the cluster CA.
func (n *Node) operatorCAPaths() (certPath, keyPath string) {
	certPath = n.cfg.MTLS.OperatorCACert
	keyPath = n.cfg.MTLS.OperatorCAKey
	if certPath != "" && keyPath != "" {
		return certPath, keyPath
	}
	// Beside the cluster CA, which is where somebody looking for
	// certificate material already goes.
	dir := filepath.Dir(n.cfg.MTLS.CACert)
	if dir == "" || dir == "." {
		dir = n.cfg.DataDir
	}
	if certPath == "" {
		certPath = filepath.Join(dir, "operator-ca.pem")
	}
	if keyPath == "" {
		keyPath = filepath.Join(dir, "operator-ca-key.pem")
	}
	return certPath, keyPath
}

// wireEnrolment builds the operator CA, trusts it for client
// certificates, and registers the operator-facing half of the service.
//
// Runs even when EnrolAddr is empty: an existing operator can still
// list and decide, and the node still trusts credentials issued
// earlier. Only the unauthenticated submit path needs the listener.
func (n *Node) wireEnrolment() error {
	if n.raft == nil || n.store == nil || n.cfg.Creds == nil {
		n.log.Debug("enrolment: no local store or credentials; not registering")
		return nil
	}
	// Enrolment is optional, and a node with no configured CA path has
	// nowhere sensible to put an operator root. Skipped rather than
	// fatal: refusing to boot because a node cannot issue OPERATOR
	// credentials would take the assistant down over an
	// administrative convenience.
	if n.cfg.MTLS.CACert == "" {
		n.log.Debug("enrolment: no cluster CA path configured; not registering")
		return nil
	}

	certPath, keyPath := n.operatorCAPaths()
	caCert, caKey, err := mtls.LoadOrCreateOperatorCA(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("operator CA: %w", err)
	}
	caPEM, err := os.ReadFile(certPath)
	if err != nil {
		return fmt.Errorf("read operator CA: %w", err)
	}
	// Trusted for CLIENT certificates only. TrustOperatorCA never
	// touches RootCAs, so this node dialling a peer still accepts the
	// cluster CA alone.
	if err := n.cfg.Creds.TrustOperatorCA(caPEM); err != nil {
		return fmt.Errorf("trust operator CA: %w", err)
	}
	clusterPEM, err := os.ReadFile(n.cfg.MTLS.CACert)
	if err != nil {
		return fmt.Errorf("read cluster CA: %w", err)
	}

	store, err := memory.NewEnrolmentStore(memory.EnrolmentStoreConfig{
		Raft: n.raft, Store: n.store, Log: n.log,
	})
	if err != nil {
		return fmt.Errorf("enrolment store: %w", err)
	}
	n.enrolments = store

	svc := &enrolmentService{
		store:      store,
		log:        n.log,
		caCert:     caCert,
		caKey:      caKey,
		clusterCA:  clusterPEM,
		operatorCA: caPEM,
		validFor:   n.cfg.MTLS.EnrolValidFor,
	}
	n.enrolmentSvc = svc
	lobslawv1.RegisterEnrolmentServiceServer(n.server, svc)
	n.log.Info("enrolment wired", "operator_ca", certPath)
	return nil
}

// startEnrolmentListener serves Submit and Poll without requiring a
// client certificate.
//
// Server-authenticated TLS: the laptop verifies the NODE using the
// cluster CA fingerprint it was given out of band, which is the one
// piece of material enrolment cannot avoid needing. Without it a
// laptop would enrol against whatever answered.
func (n *Node) startEnrolmentListener(ctx context.Context) error {
	addr := n.cfg.MTLS.EnrolAddr
	if addr == "" || n.enrolmentSvc == nil {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("enrolment listener on %s: %w", addr, err)
	}

	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(n.cfg.Creds.EnrolmentServerConfig())))
	lobslawv1.RegisterEnrolmentServiceServer(srv, &submitOnly{inner: n.enrolmentSvc})

	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()
	go func() {
		n.log.Info("enrolment listener started", "addr", addr, "client_certs", false)
		if serr := srv.Serve(ln); serr != nil && ctx.Err() == nil {
			n.log.Error("enrolment listener stopped", "err", serr)
		}
	}()
	return nil
}

// submitOnly exposes exactly two RPCs.
//
// A wrapper rather than a comment saying "do not call the others".
// List and Decide reaching an unauthenticated listener would let
// anybody enumerate pending requests and approve one, so the surface
// is narrowed structurally: the embedded Unimplemented server answers
// everything this type does not override.
type submitOnly struct {
	lobslawv1.UnimplementedEnrolmentServiceServer
	inner *enrolmentService
}

func (s *submitOnly) SubmitEnrolment(ctx context.Context, req *lobslawv1.SubmitEnrolmentRequest) (
	*lobslawv1.SubmitEnrolmentResponse, error) {
	return s.inner.SubmitEnrolment(ctx, req)
}

func (s *submitOnly) PollEnrolment(ctx context.Context, req *lobslawv1.PollEnrolmentRequest) (
	*lobslawv1.PollEnrolmentResponse, error) {
	return s.inner.PollEnrolment(ctx, req)
}

func (n *Node) sweepEnrolments(ctx context.Context, store *memory.EnrolmentStore) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			closed, err := store.Sweep(time.Now())
			if err != nil {
				n.log.Warn("enrolment sweep failed", "err", err)
				continue
			}
			if closed > 0 {
				n.log.Info("enrolment requests expired", "count", closed)
			}
		}
	}
}

// enrolmentSweeperName is the singleton key for the expiry sweep.
const enrolmentSweeperName = "enrolment-sweeper"

// startEnrolmentSweeper closes out unanswered requests on whichever
// node holds leadership.
//
// Leader-pinned for the reason the prompt sweeper is: every node
// sweeping would be correct but would burn a raft round-trip per node
// per expiry.
func (n *Node) startEnrolmentSweeper(ctx context.Context) {
	if n.enrolments == nil || n.leaderGate == nil {
		return
	}
	go func() {
		err := singleton.Run(ctx, n.leaderGate, enrolmentSweeperName, n.log,
			func(ctx context.Context) error {
				return n.sweepEnrolments(ctx, n.enrolments)
			})
		if err != nil && ctx.Err() == nil {
			n.log.Warn("enrolment sweeper stopped", "err", err)
		}
	}()
}

// attachEnrolmentAsker closes the loop between the enrolment service
// and a channel.
//
// The two need each other: the service asks the channel to put a
// question in front of somebody, and the channel asks the service to
// apply the answer. Enrolment wires first, so the service exists when
// the handler is built and only this direction needs a late setter.
//
// Called with nil when no channel is configured, which leaves
// enrolment decidable from the CLI alone.
func (n *Node) attachEnrolmentAsker(asker EnrolmentAsker) {
	if n.enrolmentSvc == nil {
		return
	}
	n.enrolmentSvc.asker = asker
	if asker == nil {
		n.log.Debug("enrolment: no channel wired; approval is CLI-only")
	}
}

// enrolmentDecider returns the service as a gateway decider, or nil.
//
// Typed nil is a real hazard here: returning n.enrolmentSvc directly
// when it is nil would hand the gateway a non-nil interface holding a
// nil pointer, and its `Enrolments == nil` guard would not fire.
func (n *Node) enrolmentDecider() gateway.EnrolmentDecider {
	if n.enrolmentSvc == nil {
		return nil
	}
	return n.enrolmentSvc
}
