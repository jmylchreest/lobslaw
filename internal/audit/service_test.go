package audit

import (
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func newServiceWithLocal(t *testing.T) (*Service, *LocalSink) {
	t.Helper()
	sink, err := NewLocalSink(LocalConfig{
		Path: filepath.Join(t.TempDir(), "audit.jsonl"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	log, err := NewAuditLog(t.Context(), Config{Sinks: []AuditSink{sink}})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(log)
	if err != nil {
		t.Fatal(err)
	}
	return svc, sink
}

func TestServiceAppendEchoesID(t *testing.T) {
	t.Parallel()
	svc, _ := newServiceWithLocal(t)
	req := &lobslawv1.AppendRequest{
		Entry: &lobslawv1.AuditEntry{
			Timestamp: timestamppb.Now(),
			Action:    "tool:exec",
			Target:    "bash",
		},
	}
	res, err := svc.Append(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Id == "" {
		t.Error("AppendResponse.Id must be set")
	}
}

func TestServiceAppendRejectsNilEntry(t *testing.T) {
	t.Parallel()
	svc, _ := newServiceWithLocal(t)
	_, err := svc.Append(t.Context(), &lobslawv1.AppendRequest{})
	if err == nil {
		t.Fatal("nil entry should fail")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v; want InvalidArgument", status.Code(err))
	}
}

func TestServiceQueryAppliesFilter(t *testing.T) {
	t.Parallel()
	svc, _ := newServiceWithLocal(t)
	for _, actor := range []string{"user:alice", "user:bob", "user:alice"} {
		_, err := svc.Append(t.Context(), &lobslawv1.AppendRequest{
			Entry: &lobslawv1.AuditEntry{
				Timestamp:  timestamppb.Now(),
				Action:     "x",
				ActorScope: actor,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	res, err := svc.Query(t.Context(), &lobslawv1.QueryRequest{
		ActorScope: "user:alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 2 {
		t.Errorf("want 2 alice entries; got %d", len(res.Entries))
	}
}

func TestServiceQueryUnknownSink(t *testing.T) {
	t.Parallel()
	svc, _ := newServiceWithLocal(t)
	_, err := svc.Query(t.Context(), &lobslawv1.QueryRequest{Sink: "bogus"})
	if err == nil {
		t.Fatal("unknown sink should fail")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v; want InvalidArgument", status.Code(err))
	}
}

func TestServiceVerifyChainClean(t *testing.T) {
	t.Parallel()
	svc, _ := newServiceWithLocal(t)
	for i := 0; i < 3; i++ {
		_, err := svc.Append(t.Context(), &lobslawv1.AppendRequest{
			Entry: &lobslawv1.AuditEntry{
				Timestamp: timestamppb.Now(),
				Action:    "x",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	res, err := svc.VerifyChain(t.Context(), &lobslawv1.VerifyChainRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ok {
		t.Errorf("chain should verify OK; first_break=%q", res.FirstBreakId)
	}
	if res.EntriesChecked != 3 {
		t.Errorf("entries_checked = %d; want 3", res.EntriesChecked)
	}
}

func TestServiceRejectsNilLog(t *testing.T) {
	t.Parallel()
	if _, err := NewService(nil); err == nil {
		t.Error("nil log should fail")
	}
}
