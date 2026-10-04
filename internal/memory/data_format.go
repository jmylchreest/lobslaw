package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	bolt "go.etcd.io/bbolt"

	"github.com/jmylchreest/lobslaw/internal/dataformat"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
)

const formatBucket = "data_format"
const formatKey = "manifest"

type StateFormat struct {
	Version  int    `json:"version"`
	Protocol string `json:"protocol"`
}

func readStateFormat(tx *bolt.Tx) (StateFormat, error) {
	var format StateFormat
	if b := tx.Bucket([]byte(formatBucket)); b != nil {
		raw := b.Get([]byte(formatKey))
		if raw == nil {
			return format, errors.New("missing state format manifest")
		}
		if err := json.Unmarshal(raw, &format); err != nil {
			return format, err
		}
		if format.Version < 1 {
			return format, errors.New("invalid state format version")
		}
	}
	return format, nil
}

func validateStateFormat(tx *bolt.Tx, cipher *crypto.Cipher, validateRecords bool) (StateFormat, error) {
	format, err := readStateFormat(tx)
	if err != nil {
		return format, err
	}
	if format.Version > dataformat.StateVersion {
		return format, fmt.Errorf("state format %d is newer than supported %d", format.Version, dataformat.StateVersion)
	}
	compatible := (format.Version == 0 && format.Protocol == "") || (format.Version == dataformat.StateVersion && format.Protocol == dataformat.ClusterProtocol) || (format.Version == 1 && format.Protocol == dataformat.PreviousStateProtocol)
	if !compatible {
		return format, fmt.Errorf("state requires protocol %q; binary supports %q", format.Protocol, dataformat.ClusterProtocol)
	}
	err = tx.ForEach(func(name []byte, b *bolt.Bucket) error {
		if string(name) == formatBucket {
			return nil
		}
		if !slices.Contains(allBuckets, string(name)) {
			return fmt.Errorf("unsupported state bucket %q; use a binary supporting the source features", name)
		}
		return b.ForEach(func(k, v []byte) error {
			if v == nil {
				return fmt.Errorf("unexpected nested bucket in %q", name)
			}
			// Notification key index is derived plaintext, rebuilt on open/restore.
			if string(name) == bucketInboxNotificationKeys {
				return nil
			}
			if validateRecords || format.Version == 0 {
				if _, err := cipher.OpenTo(nil, v); err != nil {
					return fmt.Errorf("validate encrypted state bucket %q: %w", name, err)
				}
			}
			return nil
		})
	})
	return format, err
}

// upgradeStateDB runs only on a private candidate or an empty database. The
// initial migration adds format metadata; payloads and their encryption stay
// byte-for-byte unchanged. Later migrations must join this ordered registry.
func upgradeStateDB(db *bolt.DB, cipher *crypto.Cipher) error {
	return db.Update(func(tx *bolt.Tx) error {
		format, err := validateStateFormat(tx, cipher, true)
		if err != nil {
			return err
		}
		steps := []dataformat.Step[*bolt.Tx]{
			{From: 0, To: 1, Name: "record-state-format", Apply: func(_ context.Context, tx *bolt.Tx) (*bolt.Tx, error) {
				return tx, writeStateFormat(tx, StateFormat{Version: 1, Protocol: dataformat.PreviousStateProtocol})
			}},
			{From: 1, To: 2, Name: "team-record-support", Apply: func(_ context.Context, tx *bolt.Tx) (*bolt.Tx, error) {
				return tx, writeStateFormat(tx, StateFormat{Version: dataformat.StateVersion, Protocol: dataformat.ClusterProtocol})
			}},
		}

		_, err = dataformat.Upgrade(context.Background(), tx, format.Version, dataformat.StateVersion, steps)
		return err
	})
}

func writeStateFormat(tx *bolt.Tx, format StateFormat) error {
	b, err := tx.CreateBucketIfNotExists([]byte(formatBucket))
	if err != nil {
		return err
	}
	raw, err := json.Marshal(format)
	if err != nil {
		return err
	}
	return b.Put([]byte(formatKey), raw)
}

func (s *Store) prepareFormat() error {
	db := s.loadDB()
	var format StateFormat
	var populated bool
	if err := db.View(func(tx *bolt.Tx) error {
		var err error
		format, err = validateStateFormat(tx, s.cipher, false)
		if err != nil {
			return err
		}
		return tx.ForEach(func(_ []byte, b *bolt.Bucket) error {
			if k, _ := b.Cursor().First(); k != nil {
				populated = true
			}
			return nil
		})
	}); err != nil {
		return err
	}
	if s.readOnly || format.Version == dataformat.StateVersion {
		return nil
	}
	if !populated {
		return upgradeStateDB(db, s.cipher)
	}
	// Keep an immutable rollback image. An interrupted copy is never published.
	backup, err := os.CreateTemp(filepath.Dir(s.path), "state-before-migration-*.db")
	if err != nil {
		return err
	}
	path := backup.Name()
	copyErr := db.View(func(tx *bolt.Tx) error { _, err := tx.WriteTo(backup); return err })
	if err := errors.Join(copyErr, backup.Sync(), backup.Close()); err != nil {
		return fmt.Errorf("backup state before migration: %w", err)
	}
	if err := syncSnapshotDirectory(filepath.Dir(s.path)); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return s.RestoreFromSnapshot(file)
}
