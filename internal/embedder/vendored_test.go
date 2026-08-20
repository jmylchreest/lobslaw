package embedder

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The VENDORED mirror, fetched for real.
//
// It is not enough that the release exists: the fetcher has to cope
// with a GitHub release's flat namespace, where 1_Pooling/config.json
// cannot be an asset name. This downloads it, loads it, and checks the
// pooling declaration survived the rename — which is the whole reason
// the flattened alternative exists.
//
// Opt-in: LOBSLAW_EMBEDDER_LIVE_FETCH=1.
func TestVendoredMirrorLoads(t *testing.T) {
	if os.Getenv("LOBSLAW_EMBEDDER_LIVE_FETCH") == "" {
		t.Skip("set LOBSLAW_EMBEDDER_LIVE_FETCH=1")
	}
	const base = "https://github.com/jmylchreest/lobslaw/releases/download/models-all-MiniLM-L6-v2"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dir, err := Ensure(ctx, &http.Client{Timeout: 8 * time.Minute}, t.TempDir(), "all-MiniLM-L6-v2", base)
	if err != nil {
		t.Fatalf("Ensure from the vendored mirror: %v", err)
	}
	// The rename must have been undone.
	if _, err := os.Stat(filepath.Join(dir, "1_Pooling", "config.json")); err != nil {
		t.Errorf("the flattened pooling file was not restored to its nested path: %v", err)
	}

	e, err := Open(dir)
	if err != nil {
		t.Fatalf("open the mirrored model: %v", err)
	}
	defer func() { _ = e.Close() }()

	if e.Dim() != 384 || e.VocabSize() != 30522 {
		t.Errorf("dim=%d vocab=%d, want 384/30522", e.Dim(), e.VocabSize())
	}
	// And it embeds — a mirror that downloads but produces nothing is
	// not a working mirror.
	q := e.Encode("where do I live")
	hit := e.Encode("the user is based in Yorkshire")
	miss := e.Encode("the raid array uses six disks")
	if cos(q, hit) <= cos(q, miss) {
		t.Error("the mirrored model does not rank a relevant memory above an unrelated one")
	}
}
