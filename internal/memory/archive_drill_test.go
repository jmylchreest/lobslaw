package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/jmylchreest/lobslaw/internal/archive"
	"github.com/jmylchreest/lobslaw/internal/embedder"
)

// Optional private fixture drill. The committed test contains neither personal
// records nor keys; regular CI uses the synthetic round-trip tests.
func TestArchivePrivateFixtureDrill(t *testing.T) {
	path := os.Getenv("LOBSLAW_ARCHIVE_DRILL")
	if path == "" {
		t.Skip("set LOBSLAW_ARCHIVE_DRILL and LOBSLAW_ARCHIVE_IDENTITY for a private restore drill")
	}
	keyFile, err := os.Open(os.Getenv("LOBSLAW_ARCHIVE_IDENTITY"))
	if err != nil {
		t.Fatal(err)
	}
	identities, err := age.ParseIdentities(keyFile)
	_ = keyFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := archive.Read(file, identities...)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	opts := ArchiveImportOptions{
		Owners: make(map[string]string), SourceTimezone: "UTC",
	}
	for _, record := range snapshot.Records {
		msg, err := decodeArchiveRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		value := msg.ProtoReflect()
		for _, name := range []protoreflect.Name{"owner", "user_id"} {
			field := value.Descriptor().Fields().ByName(name)
			if field != nil {
				owner := value.Get(field).String()
				if owner != "" {
					opts.Owners[owner] = owner
				}
			}
		}
	}
	node, fsm := newTestRaft(t)
	ctx := context.Background()
	var destination ReembedEmbedder = stubEmbedder{model: "drill-model"}
	if modelPath := os.Getenv("LOBSLAW_ARCHIVE_MODEL"); modelPath != "" {
		encoder, err := embedder.Open(modelPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = encoder.Close() })
		destination = archiveDrillEmbedder{encoder: encoder, model: filepath.Base(modelPath)}
	}
	result, err := ApplyArchiveImport(ctx, node, fsm.Store(), snapshot.Records, opts, destination)
	if err != nil {
		t.Fatalf("private fixture import: completed %d/%d: %v", result.Completed, result.Total, err)
	}
	if result.Completed != len(snapshot.Records) {
		t.Fatal("incomplete restore")
	}
	restored, err := fsm.Store().ArchiveRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanArchiveImport(restored, snapshot.Records, opts)
	if err != nil || len(plan.Duplicates) != len(snapshot.Records) || len(plan.Additions) != 0 {
		t.Fatalf("round trip differs: additions=%d duplicates=%d conflicts=%d err=%v",
			len(plan.Additions), len(plan.Duplicates), len(plan.Conflicts), err)
	}
	t.Logf("restored %d records through Raft into a fresh key; destination embeddings rebuilt", result.Completed)
}

type archiveDrillEmbedder struct {
	encoder *embedder.Encoder
	model   string
}

func (e archiveDrillEmbedder) Model() string { return e.model }

func (e archiveDrillEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return e.encoder.Encode(text), nil
}
