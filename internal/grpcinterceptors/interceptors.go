package grpcinterceptors

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"log/slog"
	"runtime/debug"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/logging"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
)

// RequestID returns a unary interceptor that generates a short random
// request ID, stashes a component-tagged logger carrying it in the
// request context, and logs RPC start/end at debug level.
func RequestID(base *slog.Logger) grpc.UnaryServerInterceptor {
	if base == nil {
		base = slog.Default()
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		logger := base.With(
			"rpc", info.FullMethod,
			"request_id", newRequestID(),
		)
		ctx = logging.Into(ctx, logger)
		return handler(ctx, req)
	}
}

// RequestIDStream is the streaming-RPC counterpart.
func RequestIDStream(base *slog.Logger) grpc.StreamServerInterceptor {
	if base == nil {
		base = slog.Default()
	}
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		logger := base.With(
			"rpc", info.FullMethod,
			"request_id", newRequestID(),
		)
		wrapped := &wrappedStream{ServerStream: ss, ctx: logging.Into(ss.Context(), logger)}
		return handler(srv, wrapped)
	}
}

// Recovery returns a unary interceptor that converts panics in
// downstream handlers into codes.Internal errors. The panic value
// and goroutine stack are logged via logger at error level.
func Recovery(logger *slog.Logger) grpc.UnaryServerInterceptor {
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.ErrorContext(ctx, "gRPC handler panicked",
					"rpc", info.FullMethod,
					"panic", r,
					"stack", string(debug.Stack()),
				)
				err = status.Errorf(codes.Internal, "internal server error")
			}
		}()
		return handler(ctx, req)
	}
}

// RecoveryStream is the streaming-RPC counterpart.
func RecoveryStream(logger *slog.Logger) grpc.StreamServerInterceptor {
	if logger == nil {
		logger = slog.Default()
	}
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.ErrorContext(ss.Context(), "gRPC stream panicked",
					"rpc", info.FullMethod,
					"panic", r,
					"stack", string(debug.Stack()),
				)
				err = status.Errorf(codes.Internal, "internal server error")
			}
		}()
		return handler(srv, ss)
	}
}

// newRequestID produces a short hex-encoded random string. 8 bytes
// is plenty — request IDs don't need to be globally unique, just
// distinct across concurrent RPCs on this process.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read should never fail on a sane OS; fall back to a
		// constant so logging keeps functioning.
		return "x"
	}
	return hex.EncodeToString(b[:])
}

// wrappedStream overrides Context() so downstream handlers see the
// logger-carrying context on streaming RPCs.
type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

// PeerOnlyPrefixes are the gRPC services only a cluster member may
// call.
//
// RaftTransport carries replication: append-entries, votes, snapshot
// installs. A caller reaching it is participating in consensus, not
// administering the cluster.
var PeerOnlyPrefixes = []string{"/RaftTransport/"}

// ErrOperatorNotAPeer is returned when a person's credential is used
// on a peer-only service.
var ErrOperatorNotAPeer = status.Error(codes.PermissionDenied,
	"this is an operator credential; it can administer the cluster but cannot participate in replication")

// OperatorNotAPeer refuses operator credentials on peer-only services.
//
// AN OPERATOR CERT IS ALREADY CLIENT-AUTH ONLY, so it cannot be
// presented by something answering connections. That is not sufficient
// on its own: a node dials its peers as a client too, so ClientAuth
// alone would let a laptop credential open a raft stream and take part
// in consensus.
//
// This is the half that makes "administers but cannot join" true. It
// is enforced at the SERVER, because a check on the client is a check
// the attacker controls.
//
// Unknown or absent peer information denies on the peer-only path.
// This runs on a listener where mTLS is mandatory, so a call arriving
// without a verified chain is not a configuration the cluster has —
// and guessing in favour of the caller is the wrong way to be wrong
// about consensus.
func OperatorNotAPeer() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !isPeerOnly(info.FullMethod) {
			return handler(ctx, req)
		}
		if err := denyOperator(ctx); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// OperatorNotAPeerStream is the streaming half. Raft's transport is
// streaming, so without this the interceptor guards nothing that
// matters.
func OperatorNotAPeerStream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !isPeerOnly(info.FullMethod) {
			return handler(srv, ss)
		}

		if err := denyOperator(ss.Context()); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

func isPeerOnly(fullMethod string) bool {
	for _, p := range PeerOnlyPrefixes {
		if strings.HasPrefix(fullMethod, p) {
			return true
		}
	}
	return false
}

// VerifiedPeerCert returns the leaf certificate the caller presented,
// taken from the VERIFIED chain.
//
// Not PeerCertificates: that is what the client sent, and an
// unverified certificate can claim anything. Every caller that needs
// to know who is on the other end goes through here so the
// distinction is made once rather than remembered N times.
//
// Nil when there is no verified chain, which callers must treat as
// "unidentified" rather than as an absent constraint.
func VerifiedPeerCert(ctx context.Context) *x509.Certificate {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil
	}
	if len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return nil
	}
	return tlsInfo.State.VerifiedChains[0][0]
}

func denyOperator(ctx context.Context) error {
	cert := VerifiedPeerCert(ctx)
	if cert == nil {
		return ErrOperatorNotAPeer
	}
	if mtls.IsOperatorCert(cert) {
		return ErrOperatorNotAPeer
	}
	return nil
}
