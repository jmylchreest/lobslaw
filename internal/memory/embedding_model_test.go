package memory

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/crypto"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// CHANGING THE EMBEDDING MODEL AT THE SAME WIDTH HAD NO SIGNAL.
//
// A change of WIDTH was always survivable: vectorSearch compares
// len(embedding) against the query and skips what it cannot compare,
// counting and warning as it goes. A change at the same width was not.
// Cosine across two models' vectors is meaningless but not erroneous —
// it returns a number, the number sorts, and the top hit looks like an
// answer. 1536 being the most common width, this is the likely case
// rather than the exotic one.

func putStampedVector(t *testing.T, store *Store, id, model string) {
	t.Helper()
	raw, err := proto.Marshal(&lobslawv1.VectorRecord{
		Id: id, Embedding: []float32{1, 0}, EmbeddingModel: model,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(BucketVectorRecords, id, raw); err != nil {
		t.Fatal(err)
	}
}

func TestADifferentModelAtTheSameWidthIsRefused(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	putStampedVector(t, store, "v1", "qwen3-embedding-8b")

	err := CheckEmbeddingModel(store, "text-embedding-3-small")
	if err == nil {
		t.Fatal("a model swap was accepted; every later search would be silently wrong")
	}
	var changed *ErrEmbeddingModelChanged
	if !errors.As(err, &changed) {
		t.Fatalf("wrong error type: %T", err)
	}
	// The message has to name both models and the way out, or the
	// operator's only option is to guess which one was right.
	for _, want := range []string{"qwen3-embedding-8b", "text-embedding-3-small", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestTheSameModelIsAccepted(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	putStampedVector(t, store, "v1", "qwen3-embedding-8b")

	if err := CheckEmbeddingModel(store, "qwen3-embedding-8b"); err != nil {
		t.Errorf("the configured model was refused against its own vectors: %v", err)
	}
}

// An EMPTY store is the first-enable case, and must not be refused —
// there is nothing to be incompatible with.
func TestAnEmptyStoreAcceptsAnyModel(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	if err := CheckEmbeddingModel(store, "qwen3-embedding-8b"); err != nil {
		t.Errorf("an empty store refused a model: %v", err)
	}
}

// Records written BEFORE the field existed carry no stamp. Refusing to
// boot over a record whose model nobody can determine would strand a
// node with no way to fix it — unknown is not the same as mismatched.
func TestUnstampedLegacyVectorsDoNotBlockBoot(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	putStampedVector(t, store, "legacy", "")

	if err := CheckEmbeddingModel(store, "qwen3-embedding-8b"); err != nil {
		t.Errorf("legacy vectors blocked boot: %v", err)
	}
}

// A record this node cannot decrypt must not stop it booting.
//
// The guard walks vector records looking for a model stamp, and
// aborting on a decrypt failure turned an unreadable record — one
// written under a rotated key, or a corrupt page — into a node that
// never starts again, reporting a decryption error where the operator
// was expecting a model check. Nothing else at start-up walks that
// bucket, so this guard was the only thing making it fatal, and it
// made it fatal for every future boot.
//
// Simulates the real case rather than a synthetic one: records written
// under one key, then opened under another.
func TestAnUndecryptableRecordDoesNotBlockBoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	var keyA, keyB crypto.Key
	keyA[0], keyB[0] = 1, 2

	written, err := OpenStore(path, keyA)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := proto.Marshal(&lobslawv1.VectorRecord{
		Id: "v1", Embedding: []float32{1, 0}, EmbeddingModel: "qwen3-embedding-8b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := written.Put(BucketVectorRecords, "v1", raw); err != nil {
		t.Fatal(err)
	}
	if err := written.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopened under a DIFFERENT key: nothing in it can be read.
	rotated, err := OpenStore(path, keyB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rotated.Close() }()

	// Unknown, not fatal — the node must still start.
	if err := CheckEmbeddingModel(rotated, "all-MiniLM-L6-v2"); err != nil {
		t.Errorf("an unreadable store blocked boot: %v", err)
	}
}
