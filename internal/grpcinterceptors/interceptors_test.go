package grpcinterceptors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/logging"
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
