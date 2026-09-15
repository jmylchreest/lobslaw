package memory

import (
	"errors"
	"fmt"

	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

const maxArchiveBatchBytes = 8 << 20

// applyArchiveBatch commits knowledge and the resume receipt atomically. A
// replay checks the receipt before touching records, including ones edited or
// deleted after import. New writes require absence at the transaction boundary.
func (f *FSM) applyArchiveBatch(batch *lobslawv1.ArchiveBatch) error {
	if batch == nil || batch.ImportId == "" || batch.BatchId == "" || len(batch.Receipt) == 0 {
		return errors.New("archive batch requires import id, batch id and receipt")
	}
	if proto.Size(batch) > maxArchiveBatchBytes {
		return errors.New("archive batch exceeds size limit")
	}
	key := batch.ImportId + "/" + batch.BatchId
	touched := make(map[string]bool)
	err := f.store.loadDB().Update(func(tx *bolt.Tx) error {
		journal := tx.Bucket([]byte(BucketArchiveImports))
		if existing := journal.Get([]byte(key)); existing != nil {
			raw, err := f.store.cipher.OpenTo(nil, existing)
			if err != nil {
				return err
			}
			if string(raw) != string(batch.Receipt) {
				return errors.New("archive receipt collision")
			}
			return nil
		}
		for _, mutation := range batch.Records {
			bucket, err := f.applyArchiveMutation(tx, mutation)
			if err != nil {
				return err
			}
			if bucket != "" {
				touched[bucket] = true
			}
		}
		return putArchiveValue(tx, f.store, BucketArchiveImports, key, batch.Receipt)
	})
	if err != nil {
		return err
	}
	if (touched[BucketScheduledTasks] || touched[BucketCommitments]) && f.schedulerChange != nil {
		f.schedulerChange()
	}
	if touched[BucketSoulTune] && f.soulTuneChange != nil {
		f.soulTuneChange()
	}
	if (touched[BucketSelfTaught] || touched[BucketSelfTaughtArchive]) && f.selfTaughtChange != nil {
		f.selfTaughtChange()
	}
	return nil
}

func putArchiveValue(tx *bolt.Tx, store *Store, bucket, key string, raw []byte) error {
	sealed, err := store.cipher.Seal(raw)
	if err != nil {
		return err
	}
	return tx.Bucket([]byte(bucket)).Put([]byte(key), sealed)
}

func findArchiveKind(name string) (archiveKind, error) {
	for _, kind := range archiveKinds {
		if kind.kind == name {
			return kind, nil
		}
	}
	return archiveKind{}, fmt.Errorf("unsupported archive kind %q", name)
}

func (f *FSM) applyArchiveMutation(tx *bolt.Tx, mutation *lobslawv1.ArchiveMutation) (string, error) {
	kind, err := findArchiveKind(mutation.Kind)
	if err != nil {
		return "", err
	}
	if mutation.Id == "" {
		return "", errors.New("archive mutation requires id")
	}
	bucket := tx.Bucket([]byte(kind.bucket))
	existing := bucket.Get([]byte(mutation.Id))
	if mutation.DependencyOnly {
		if existing == nil {
			return "", errors.New("archive dependency disappeared; re-plan import")
		}
		raw, err := f.store.cipher.OpenTo(nil, existing)
		if err != nil {
			return "", err
		}
		if Digest(raw) != mutation.ExpectedDigest {
			return "", errors.New("archive dependency changed; re-plan import")
		}
		return "", nil
	}
	if existing != nil {
		return "", fmt.Errorf("archive target %s/%s changed; re-plan import", kind.kind, mutation.Id)
	}
	msg := proto.Clone(kind.message)
	if err := proto.Unmarshal(mutation.Payload, msg); err != nil {
		return "", err
	}
	if mapping, ok := msg.(*lobslawv1.ArchiveMapping); ok && mutation.ExpectedDigest != "" {
		target, err := findArchiveKind(mapping.Kind)
		if err != nil {
			return "", err
		}
		sealed := tx.Bucket([]byte(target.bucket)).Get([]byte(mapping.DestinationId))
		if sealed == nil {
			return "", errors.New("mapped destination disappeared; re-plan import")
		}
		raw, err := f.store.cipher.OpenTo(nil, sealed)
		if err != nil {
			return "", err
		}
		current := proto.Clone(target.message)
		if err := proto.Unmarshal(raw, current); err != nil {
			return "", err
		}
		digest, err := archiveFingerprint(current)
		if err != nil {
			return "", err
		}
		if digest != mutation.ExpectedDigest || digest != mapping.DestinationDigest {
			return "", errors.New("mapped destination changed; re-plan import")
		}
	}
	setRevision(msg, 1)
	if vector, ok := msg.(*lobslawv1.VectorRecord); ok {
		vector.Norm = norm(vector.Embedding)
	}
	raw, err := proto.Marshal(msg)
	if err != nil {
		return "", err
	}
	if err := putArchiveValue(tx, f.store, kind.bucket, mutation.Id, raw); err != nil {
		return "", err
	}
	if rec, ok := msg.(*lobslawv1.ConsolidationRecord); ok {
		if rec.Verdict == string(VerdictConflict) || rec.Verdict == string(VerdictSupersedes) {
			for _, source := range rec.SourceIds {
				if source != "" {
					if err := putArchiveValue(tx, f.store, BucketDisputes, source+"/"+rec.Id, []byte(rec.Id)); err != nil {
						return "", err
					}
				}
			}
		}
	}
	return kind.bucket, nil
}
