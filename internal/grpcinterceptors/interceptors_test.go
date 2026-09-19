package grpcinterceptors

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/logging"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
)

func TestRequestIDAttachesLoggerWithID(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, nil))

	var sawID string
	handler := func(ctx context.Context, req any) (any, error) {
		logger := logging.From(ctx)
		logger.Info("in-handler", "flag", "seen")

		// Parse the log line to confirm the request_id attr is present.
		dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
		for {
			var entry map[string]any
			if err := dec.Decode(&entry); err != nil {
				if !errors.Is(err, io.EOF) {
					t.Fatal(err)
				}
				break
			}
			if entry["msg"] == "in-handler" {
				if id, ok := entry["request_id"].(string); ok {
					sawID = id
				}
			}
		}
		return "ok", nil
	}

	ic := RequestID(base)
	_, err := ic(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/test.Svc/Method"},
		handler)
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if sawID == "" {
		t.Error("request_id not attached to context logger")
	}
	if len(sawID) != 16 {
		t.Errorf("request_id %q length = %d, want 16 hex chars (8 bytes)", sawID, len(sawID))
	}
}

func TestRecoveryConvertsPanicToInternalError(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	panicHandler := func(ctx context.Context, req any) (any, error) {
		panic("deliberate test panic")
	}

	ic := Recovery(logger)
	resp, err := ic(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/test.Svc/Boom"},
		panicHandler)
	if resp != nil {
		t.Errorf("resp should be nil, got %v", resp)
	}
	if err == nil {
		t.Fatal("Recovery should convert panic to error")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("err is not a gRPC status: %v", err)
	}
	if st.Code() != codes.Internal {
		t.Errorf("code = %v, want Internal", st.Code())
	}
	if !strings.Contains(buf.String(), "deliberate test panic") {
		t.Error("panic value should be logged")
	}
	if !strings.Contains(buf.String(), "stack") {
		t.Error("panic stack should be logged")
	}
}

func TestRecoveryPassesThroughNormalErrors(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	want := errors.New("genuine error, not a panic")

	ic := Recovery(logger)
	_, err := ic(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/test.Svc/M"},
		func(ctx context.Context, req any) (any, error) { return nil, want })
	if !errors.Is(err, want) {
		t.Errorf("Recovery ate a real error: got %v, want %v", err, want)
	}
}

// An operator credential is ClientAuth-only, so nothing can SERVE with
// it. That is not sufficient on its own: a node dials its peers as a
// client too, so ClientAuth alone would let a laptop credential open a
// raft stream and take part in consensus.
//
// This is the half that makes "administers but cannot join" true, and
// it is enforced at the SERVER — a check on the client is one the
// attacker controls.

func certWithOU(t *testing.T, ou ...string) *x509.Certificate {
	t.Helper()
	return &x509.Certificate{Subject: pkix.Name{
		CommonName:         "alice",
		OrganizationalUnit: ou,
	}}
}

// ctxWithCert builds a context carrying a VERIFIED chain, which is
// what the interceptor reads — PeerCertificates is what the client
// sent, and an unverified certificate can claim anything.
func ctxWithCert(cert *x509.Certificate) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
			VerifiedChains: [][]*x509.Certificate{{cert}},
		}},
	})
}

func TestAnOperatorIsRefusedOnTheRaftTransport(t *testing.T) {
	t.Parallel()
	called := false
	h := func(context.Context, any) (any, error) { called = true; return "ok", nil }

	_, err := OperatorNotAPeer()(
		ctxWithCert(certWithOU(t, mtls.OperatorOU)), nil,
		&grpc.UnaryServerInfo{FullMethod: "/RaftTransport/AppendEntries"}, h)
	if err == nil {
		t.Fatal("an operator credential reached the raft transport")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", status.Code(err))
	}
	if called {
		t.Error("the handler ran before the refusal")
	}
}

func TestOperatorCannotAssertTurnIdentity(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"/lobslaw.v1.AgentService/RunTurn", "/lobslaw.v1.AgentService/ResumeTurn", "/lobslaw.v1.ConsoleService/ConsoleForward"} {
		called := false
		_, err := OperatorNotAPeer()(ctxWithCert(certWithOU(t, mtls.OperatorOU)), nil, &grpc.UnaryServerInfo{FullMethod: method}, func(context.Context, any) (any, error) { called = true; return nil, nil })
		if status.Code(err) != codes.PermissionDenied || called {
			t.Fatalf("%s accepted operator assertion", method)
		}
	}
}

// Raft's transport is STREAMING. Without this half the guard covers
// nothing that matters.
func TestAnOperatorIsRefusedOnTheRaftStream(t *testing.T) {
	t.Parallel()
	called := false
	h := func(any, grpc.ServerStream) error { called = true; return nil }

	err := OperatorNotAPeerStream()(nil,
		fakeStream{ctx: ctxWithCert(certWithOU(t, mtls.OperatorOU))},
		&grpc.StreamServerInfo{FullMethod: "/RaftTransport/AppendEntriesPipeline"}, h)
	if err == nil {
		t.Fatal("an operator credential opened a raft stream")
	}
	if called {
		t.Error("the handler ran before the refusal")
	}
}

// The whole point of the credential is that it CAN administer.
func TestAnOperatorReachesTheAdministrativeServices(t *testing.T) {
	t.Parallel()
	for _, method := range []string{
		"/lobslaw.v1.SkillService/ImportSkill",
		"/lobslaw.v1.MemoryService/Query",
		"/lobslaw.v1.PolicyService/ListRules",
	} {
		called := false
		h := func(context.Context, any) (any, error) { called = true; return "ok", nil }
		if _, err := OperatorNotAPeer()(
			ctxWithCert(certWithOU(t, mtls.OperatorOU)), nil,
			&grpc.UnaryServerInfo{FullMethod: method}, h); err != nil {
			t.Errorf("%s: an operator was refused: %v", method, err)
		}
		if !called {
			t.Errorf("%s: the handler did not run", method)
		}
	}
}

// A node must still replicate, or this secures the laptop by breaking
// the cluster.
func TestANodeStillReachesTheRaftTransport(t *testing.T) {
	t.Parallel()
	called := false
	h := func(context.Context, any) (any, error) { called = true; return "ok", nil }

	if _, err := OperatorNotAPeer()(
		ctxWithCert(certWithOU(t)), nil,
		&grpc.UnaryServerInfo{FullMethod: "/RaftTransport/AppendEntries"}, h); err != nil {
		t.Fatalf("a node was refused replication: %v", err)
	}
	if !called {
		t.Error("the handler did not run for a node")
	}
}

// A call with no verified chain is not a configuration this cluster
// has — mTLS is mandatory on this listener. Guessing in favour of the
// caller is the wrong way to be wrong about consensus.
func TestUnidentifiedCallersAreRefusedOnThePeerPath(t *testing.T) {
	t.Parallel()
	h := func(context.Context, any) (any, error) { return "ok", nil }
	info := &grpc.UnaryServerInfo{FullMethod: "/RaftTransport/AppendEntries"}

	for name, ctx := range map[string]context.Context{
		"no peer at all": context.Background(),
		"no TLS info":    peer.NewContext(context.Background(), &peer.Peer{}),
		"no verified chain": peer.NewContext(context.Background(), &peer.Peer{
			AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{}},
		}),
	} {
		if _, err := OperatorNotAPeer()(ctx, nil, info, h); err == nil {
			t.Errorf("%s: reached the raft transport", name)
		}
	}
}

// The same caller with no identity is fine on the administrative
// services, where the listener's own mTLS is the gate — the peer-only
// path is the one with the extra rule.
func TestAnUnidentifiedCallerIsNotBlockedElsewhere(t *testing.T) {
	t.Parallel()
	h := func(context.Context, any) (any, error) { return "ok", nil }
	if _, err := OperatorNotAPeer()(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/lobslaw.v1.NodeService/Health"}, h); err != nil {
		t.Errorf("an ordinary call was refused: %v", err)
	}
}

type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f fakeStream) Context() context.Context { return f.ctx }
