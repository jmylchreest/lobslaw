package memory

import (
	"context"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/jmylchreest/lobslaw/internal/archive"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

const (
	archiveLockTimeout = time.Second
	archiveSourceMode  = 0o600
)

type archiveKind struct {
	bucket  string
	kind    string
	message proto.Message
}

// An allowlist makes a new runtime/credential bucket non-exportable by default.
var archiveKinds = []archiveKind{
	{BucketEpisodicRecords, "episodic", &lobslawv1.EpisodicRecord{}},
	{BucketVectorRecords, "documents", &lobslawv1.VectorRecord{}},
	{BucketConsolidations, "consolidations", &lobslawv1.ConsolidationRecord{}},
	{BucketPinned, "pinned", &lobslawv1.PinnedMemory{}},
	{BucketSoulTune, "soul", &lobslawv1.SoulTuneRecord{}},
	{BucketSkills, "skills", &lobslawv1.SkillRecord{}},
	{BucketSkillBlobs, "skill-blobs", &lobslawv1.SkillBlob{}},
	{BucketSelfTaught, "learned", &lobslawv1.SelfTaughtRecord{}},
	{BucketSelfTaughtArchive, "learned-archive", &lobslawv1.SelfTaughtRecord{}},
	{BucketSelfTaughtHistory, "learned-history", &lobslawv1.SelfTaughtRecord{}},
	{BucketSessions, "sessions", &lobslawv1.SessionRecord{}},
	{BucketSessionMessages, "session-messages", &lobslawv1.SessionMessage{}},
	{BucketUserPrefs, "preferences", &lobslawv1.UserPreferences{}},
	{BucketScheduledTasks, "scheduled-tasks", &lobslawv1.ScheduledTaskRecord{}},
	{BucketCommitments, "commitments", &lobslawv1.AgentCommitment{}},
}

// ReadArchiveRecords opens an EXISTING database read-only, unlike OpenStore,
// which creates buckets. A running writer's exclusive lock makes this fail.
// Every selected bucket is read in one transaction, including skill blobs.
func ReadArchiveRecords(ctx context.Context, path string, key crypto.Key) ([]archive.Record, error) {
	db, err := bolt.Open(path, archiveSourceMode, &bolt.Options{
		ReadOnly: true,
		Timeout:  archiveLockTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("open archive source (stop node or use snapshot): %w", err)
	}
	defer func() { _ = db.Close() }()
	cipher, err := crypto.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return readArchiveRecords(ctx, db, cipher)
}

// ArchiveRecords reads a live store in one consistent transaction.
func (s *Store) ArchiveRecords(ctx context.Context) ([]archive.Record, error) {
	return readArchiveRecords(ctx, s.loadDB(), s.cipher)
}

func readArchiveRecords(ctx context.Context, db *bolt.DB, cipher *crypto.Cipher) ([]archive.Record, error) {
	var records []archive.Record
	var total int64
	err := db.View(func(tx *bolt.Tx) error {
		for _, kind := range archiveKinds {
			b := tx.Bucket([]byte(kind.bucket))
			if b == nil {
				continue
			}
			if err := b.ForEach(func(k, v []byte) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				if len(records) >= archive.MaxRecords || int64(len(v)) > archive.MaxRecordBytes {
					return errors.New("archive source exceeds size limit")
				}
				data, err := kind.encode(cipher, v)
				if err != nil {
					return err
				}

				total += int64(len(data))
				if total > archive.MaxArchiveBytes || int64(len(data)) > archive.MaxRecordBytes {
					return errors.New("archive source exceeds size limit")
				}
				records = append(records, archive.Record{
					Kind: kind.kind,
					ID:   string(k),
					Data: data,
				})
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

func (kind archiveKind) encode(cipher *crypto.Cipher, sealed []byte) ([]byte, error) {
	raw, err := cipher.OpenTo(nil, sealed)
	if err != nil {
		return nil, fmt.Errorf("decrypt archive bucket %s: %w", kind.bucket, err)
	}

	msg := proto.Clone(kind.message)
	if err := proto.Unmarshal(raw, msg); err != nil {
		return nil, fmt.Errorf("decode archive bucket %s: %w", kind.bucket, err)
	}
	if err := portableMessage(msg.ProtoReflect()); err != nil {
		return nil, err
	}
	return (protojson.MarshalOptions{UseProtoNames: true}).Marshal(msg)
}

func portableMessage(msg protoreflect.Message) error {
	if len(msg.GetUnknown()) > 0 {
		return errors.New("source contains unknown protobuf fields; export with a newer binary")
	}
	for _, name := range []protoreflect.Name{"embedding", "norm", "embedding_model", "claimed_by", "claim_expires_at", "revision"} {
		if f := msg.Descriptor().Fields().ByName(name); f != nil {
			msg.Clear(f)
		}
	}
	var found error
	msg.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case f.IsMap():
			if f.MapValue().Message() != nil {
				v.Map().Range(func(_ protoreflect.MapKey, v protoreflect.Value) bool {
					found = portableMessage(v.Message())
					return found == nil
				})
			}
		case f.IsList():
			if f.Message() != nil {
				for i := 0; i < v.List().Len(); i++ {
					if found = portableMessage(v.List().Get(i).Message()); found != nil {
						break
					}
				}
			}
		case f.Message() != nil:
			found = portableMessage(v.Message())
		}
		return found == nil
	})
	return found
}
