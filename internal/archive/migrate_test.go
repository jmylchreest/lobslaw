package archive

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestLegacyArchiveUpgradePreservesProvenanceAndRecords(t *testing.T) {
	source := Snapshot{Manifest: Manifest{Format: Format, SchemaVersion: 1, SnapshotID: "old", CreatedAt: time.Now(), Counts: map[string]int{}, Checksums: map[string]string{}}, Records: []Record{{Kind: "sessions", ID: "old", Data: json.RawMessage(`{"id":"old"}`)}}}
	upgraded, err := Upgrade(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Manifest.SchemaVersion != SchemaVersion || source.Manifest.SchemaVersion != 1 {
		t.Fatal("version not normalized on copy")
	}
	if upgraded.Manifest.SourceID != "" {
		t.Fatal("invented owner/source mapping")
	}
	upgraded.Records[0].Data[0] = 'x'
	if source.Records[0].Data[0] != '{' {
		t.Fatal("mutated source records")
	}
	source.Manifest.SchemaVersion = 999
	if _, err := Upgrade(context.Background(), source); err == nil {
		t.Fatal("accepted future archive")
	}
}
