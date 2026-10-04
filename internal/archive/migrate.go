package archive

import (
	"context"
	"encoding/json"

	"github.com/jmylchreest/lobslaw/internal/dataformat"
)

// Upgrade normalizes an authenticated snapshot in memory. Read preserves source
// metadata for inspection; import adapters explicitly upgrade only after Read
// has verified the complete archive. Source bytes and input records are untouched.
func Upgrade(ctx context.Context, source Snapshot) (Snapshot, error) {
	copy := source
	copy.Records = make([]Record, len(source.Records))
	for i, r := range source.Records {
		copy.Records[i] = r
		copy.Records[i].Data = append(json.RawMessage(nil), r.Data...)
	}
	steps := []dataformat.Step[Snapshot]{{From: 1, To: 2, Name: "optional-stable-source-identity", Apply: func(_ context.Context, s Snapshot) (Snapshot, error) {
		// Version 2 introduced optional SourceID. Never invent identity for v1: the
		// existing import service still requires an explicit operator mapping.
		s.Manifest.SchemaVersion = 2
		return s, nil
	}}}
	if err := validateManifest(source.Manifest); err != nil {
		return Snapshot{}, err
	}
	return dataformat.Upgrade(ctx, copy, source.Manifest.SchemaVersion, SchemaVersion, steps)
}
