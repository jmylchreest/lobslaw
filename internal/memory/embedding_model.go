package memory

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

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
	err := store.ForEach(BucketVectorRecords, func(_ string, raw []byte) error {
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
		// A RECORD WE CANNOT DECRYPT IS UNKNOWN, NOT FATAL.
		//
		// This guard's job is to read a model stamp, not to validate
		// the whole store. Aborting turned any single undecryptable
		// vector — one written under a rotated key, one corrupt page —
		// into a node that will not boot, reporting a decryption error
		// where the operator expected a model check. Nothing else at
		// start-up walks this bucket, so the guard was the only thing
		// making it fatal, and it made it fatal for every future boot.
		//
		// Unknown is the same answer an unstamped legacy record gets:
		// it never counts as a match, so a genuine model change is
		// still caught by any record that DOES decrypt.
		if strings.Contains(err.Error(), "decrypt") {
			return found, errUndecryptable
		}
		return "", fmt.Errorf("scan vector records: %w", err)
	}
	return found, nil
}

// errUndecryptable reports that the scan stopped early because a record
// could not be decrypted. The stamp found before that point, if any, is
// still returned and still usable.
var errUndecryptable = errors.New("some vector records could not be decrypted")

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
			"results. Either restore model = %q, or stop the node and re-embed with "+
			"`go run ./cmd/backfill-embeddings --force`",
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
		if !errors.Is(err, errUndecryptable) {
			return err
		}
		// Logged rather than swallowed: a store with records this node
		// cannot read is worth knowing about, and it means the check
		// below saw only part of the corpus.
		slog.Default().Warn("memory: some vector records could not be decrypted; "+
			"the embedding-model check saw only the records that could",
			"checked_model", configured)
	}
	if stored == "" || stored == configured {
		return nil
	}
	return &ErrEmbeddingModelChanged{Configured: configured, Stored: stored}
}
