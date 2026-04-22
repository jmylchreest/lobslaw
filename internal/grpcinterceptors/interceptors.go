package grpcinterceptors

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"runtime/debug"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/logging"
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
