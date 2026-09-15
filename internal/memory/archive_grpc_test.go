package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/jmylchreest/lobslaw/internal/archive"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestArchiveRPCRequiresAuthorizationAndPlansBeforeApply(t *testing.T) {
	svc := newTestServiceStack(t)
	svc.SetEmbedder(stubEmbedder{model: "destination"})
	var allowed atomic.Bool
	rpc := NewArchiveRPC(svc, func(context.Context, string) error {
		if !allowed.Load() {
			return status.Error(codes.PermissionDenied, "not authorized")
		}
		return nil
	}, nil)
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	lobslawv1.RegisterArchiveServiceServer(server, rpc)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///archive-test",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := lobslawv1.NewArchiveServiceClient(conn)
	ctx := context.Background()
	export, err := client.ExportArchive(ctx, &lobslawv1.ArchiveExportRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := export.Recv(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unauthorized export: %v", err)
	}
	allowed.Store(true)
	var payload bytes.Buffer
	if err := archive.Write(&payload, archive.Snapshot{
		Manifest: archive.Manifest{SnapshotID: "rpc-test", CreatedAt: time.Now()},
		Records:  []archive.Record{archiveTestRecord(t, "documents", "v", &lobslawv1.VectorRecord{Id: "v", Text: "summary"})},
	}); err != nil {
		t.Fatal(err)
	}
	for _, apply := range []bool{false, true} {
		stream, err := client.ImportArchive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&lobslawv1.ArchiveChunk{OptionsJson: []byte(`{}`), Apply: apply}); err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&lobslawv1.ArchiveChunk{Data: payload.Bytes()}); err != nil {
			t.Fatal(err)
		}
		response, err := stream.CloseAndRecv()
		if err != nil || response.GetError() != "" {
			t.Fatalf("import: %v, %s", err, response.GetError())
		}
		if !apply {
			if _, err := svc.store.Get(BucketVectorRecords, "v"); err == nil {
				t.Fatal("preview wrote data")
			}
		} else {
			var result ArchiveImportResult
			if err := json.Unmarshal(response.ResultJson, &result); err != nil || result.Applied != 1 {
				t.Fatalf("apply result: %+v, %v", result, err)
			}
		}
	}
	export, err = client.ExportArchive(ctx, &lobslawv1.ArchiveExportRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var restored bytes.Buffer
	for {
		chunk, err := export.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		restored.Write(chunk.Data)
	}
	snapshot, err := archive.Read(&restored)
	if err != nil || len(snapshot.Records) != 1 {
		t.Fatalf("live export: %v", err)
	}
}
