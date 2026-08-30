package compute

import (
	"context"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// The lexical-search tests exercise context_engine.go, which lives
// here; the same two fixtures serve the memory tools in [tools].
// Duplicated rather than shared because a test helper is not worth an
// exported package, and the tools package imports this one.

func newMemoryStoreForTest(t *testing.T) *memory.Store {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(t.TempDir(), "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// seedEpisodic writes a record directly via the store (bypassing
// Raft) so tests don't need a full cluster for search-only cases.
func seedEpisodic(t *testing.T, store *memory.Store, rec *lobslawv1.EpisodicRecord) {
	t.Helper()
	// Owned, because production always is: an unowned record is
	// readable by nobody, so an unowned fixture tests nothing.
	if rec.Owner == "" {
		rec.Owner = "user:alice"
		rec.Visibility = lobslawv1.Visibility_VISIBILITY_PRIVATE
	}
	raw, err := proto.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(memory.BucketEpisodicRecords, rec.Id, raw); err != nil {
		t.Fatal(err)
	}
}

type fixedEmbedder struct{}

func seedVector(t *testing.T, store *memory.Store, rec *lobslawv1.VectorRecord) {
	t.Helper()
	raw, err := proto.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(memory.BucketVectorRecords, rec.Id, raw); err != nil {
		t.Fatal(err)
	}
}

func operatorTurn(ctx context.Context) context.Context {
	return turn.WithIdentity(ctx, turn.Identity{
		UserID:    "alice",
		Principal: identity.User("alice"),
		Scope:     "default",
		Roles:     []string{"operator"},
	})
}

func (f fixedEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return f.Embed(ctx, text)
}

func (fixedEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{1, 0}, nil
}

func (fixedEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func (fixedEmbedder) Dimensions() int { return 2 }

func (fixedEmbedder) Model() string { return "test-embedder-v1" }
