package memory

import (
	"fmt"
	"log/slog"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// StoredEmbeddingModel reports which model wrote the vectors already in
// the store, or "" when nothing has been written yet.
//
// Scans until it finds the first stamped record rather than reading all
// of them: the guard's question is "does this store already belong to a
// different model", and one answer settles it. Records written before
// the field existed carry no stamp and are skipped — they are unknown,
// not a match, so a store of only-legacy vectors reads as empty and the
// guard stays quiet. That is the right way round: refusing to boot over
// a record whose model nobody can determine would strand a node with no
// way to fix it.
func StoredEmbeddingModel(store *Store) (string, error) {
	var found string
	// Tolerant: a record this node cannot read is UNKNOWN, not fatal.
	// Aborting here turned one unreadable record — written under a
	// rotated key, or a corrupt page — into a node that never starts
	// again, reporting a decryption error where the operator expected
	// a model check.
	skipped, err := store.ForEachDecryptable(BucketVectorRecords, func(_ string, raw []byte) error {
		if found != "" {
			return nil
		}
		var rec lobslawv1.VectorRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			return nil
		}
		if rec.EmbeddingModel != "" {
			found = rec.EmbeddingModel
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("scan vector records: %w", err)
	}
	if skipped > 0 {
		slog.Default().Warn("memory: vector records could not be decrypted and were skipped "+
			"while looking for the embedding-model stamp", "skipped", skipped)
	}
	return found, nil
}

// ErrEmbeddingModelChanged is returned when the configured embedding
// model disagrees with what wrote the vectors already in the store.
//
// A CHANGE OF WIDTH was always survivable: vectorSearch skips records
// it cannot compare and says so. A change at the SAME width was not.
// Cosine between two models' vectors is meaningless, but it is not
// erroneous — it returns a number, the number sorts, and the top hit
// looks like an answer. Every search would be confidently wrong with
// nothing anywhere reporting a problem.
//
// So this is fatal at boot rather than a warning. A node that will not
// start is a bad morning; a node that quietly recalls the wrong
// memories for a month is worse, and nothing about it looks broken.
type ErrEmbeddingModelChanged struct {
	Configured string
	Stored     string
}

func (e *ErrEmbeddingModelChanged) Error() string {
	return fmt.Sprintf(
		"embedding model changed: this store's vectors were written by %q but [compute.embeddings] model is %q. "+
			"Vectors from different models are not comparable, so every search would return confidently wrong "+
			"results. Either restore model = %q, or start once with "+
			"--allow-embedding-model-change and run `lobslaw memory reembed`",
		e.Stored, e.Configured, e.Stored)
}

// CheckEmbeddingModel fails when the configured model is not the one
// that wrote the store's existing vectors.
func CheckEmbeddingModel(store *Store, configured string) error {
	if store == nil || configured == "" {
		return nil
	}
	stored, err := StoredEmbeddingModel(store)
	if err != nil {
		return err
	}
	if stored == "" || stored == configured {
		return nil
	}
	return &ErrEmbeddingModelChanged{Configured: configured, Stored: stored}
}
