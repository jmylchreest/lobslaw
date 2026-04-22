package memory

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Store wraps the application-state bbolt file (state.db) with
// at-rest encryption via nacl/secretbox. Values are encrypted before
// they hit disk; callers see plaintext.
type Store struct {
	db  *bolt.DB
	key crypto.Key
}

// OpenStore opens (and creates if missing) the state.db file at path.
// Every configured bucket is ensured on open. The key is used to
// encrypt/decrypt every value.
func OpenStore(path string, key crypto.Key) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create state.db parent dir: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open state.db %q: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range allBuckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return fmt.Errorf("create bucket %q: %w", name, err)
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, key: key}, nil
}

// Close closes the underlying bbolt database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Put encrypts value and writes it at bucket/key.
func (s *Store) Put(bucket, key string, value []byte) error {
	sealed, err := crypto.Seal(s.key, value)
	if err != nil {
		return fmt.Errorf("seal %s/%s: %w", bucket, key, err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("bucket %q not found", bucket)
		}
		return b.Put([]byte(key), sealed)
	})
}

// Get reads and decrypts bucket/key. Returns types.ErrNotFound on miss.
func (s *Store) Get(bucket, key string) ([]byte, error) {
	var sealed []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("bucket %q not found", bucket)
		}
		raw := b.Get([]byte(key))
		if raw == nil {
			return types.ErrNotFound
		}
		// Copy — bbolt forbids using the slice outside the transaction.
		sealed = append(sealed[:0], raw...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	plaintext, err := crypto.Open(s.key, sealed)
	if err != nil {
		return nil, fmt.Errorf("decrypt %s/%s: %w", bucket, key, err)
	}
	return plaintext, nil
}

// Delete is idempotent — no error on missing key. Required by log
// replay semantics: re-applying a delete entry must not fail.
func (s *Store) Delete(bucket, key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("bucket %q not found", bucket)
		}
		return b.Delete([]byte(key))
	})
}

// ForEach walks every key/value pair in bucket, calling fn with the
// decrypted plaintext. fn may not retain the value beyond its return
// (the slice is valid only within the enclosing transaction).
func (s *Store) ForEach(bucket string, fn func(key string, value []byte) error) error {
	return s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("bucket %q not found", bucket)
		}
		return b.ForEach(func(k, v []byte) error {
			plaintext, err := crypto.Open(s.key, v)
			if err != nil {
				return fmt.Errorf("decrypt %s/%s: %w", bucket, string(k), err)
			}
			return fn(string(k), plaintext)
		})
	})
}

// WriteSnapshot dumps the entire state.db to w via bbolt's
// Tx.WriteTo. The dump is a self-consistent snapshot at the
// transaction boundary.
//
// Callers use this from raft.FSMSnapshot.Persist.
func (s *Store) WriteSnapshot(w io.Writer) error {
	return s.db.View(func(tx *bolt.Tx) error {
		_, err := tx.WriteTo(w)
		return err
	})
}

// RestoreFromSnapshot replaces state.db's contents with the bbolt
// dump read from r. The Store is closed and reopened; callers must
// use the returned *Store for subsequent operations.
func (s *Store) RestoreFromSnapshot(r io.Reader) (*Store, error) {
	if s.db == nil {
		return nil, errors.New("store already closed")
	}
	path := s.db.Path()
	if err := s.db.Close(); err != nil {
		return nil, fmt.Errorf("close state.db: %w", err)
	}
	tmp := path + ".restore.tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return nil, fmt.Errorf("create tmp snapshot file: %w", err)
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("write snapshot: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("close tmp snapshot: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("rename snapshot into place: %w", err)
	}
	return OpenStore(path, s.key)
}
