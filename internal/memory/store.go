package memory

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Store wraps the application-state bbolt file (state.db) with
// at-rest encryption via nacl/secretbox. Values are encrypted before
// they hit disk; callers see plaintext.
//
// The underlying *bolt.DB is held behind atomic.Pointer so a raft
// snapshot restore (which closes + reopens the file) can swap the
// handle without invalidating outside references. Without this, the
// FSM's RestoreFromSnapshot call closed the DB and every other
// component (policy engine, scheduler, services) was left holding
// a Store whose db field pointed at a closed handle — producing
// "database not open" on every subsequent operation.
type Store struct {
	db  atomic.Pointer[bolt.DB]
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
	s := &Store{key: key}
	s.db.Store(db)
	return s, nil
}

// loadDB returns the live *bolt.DB. Per atomic.Pointer semantics,
// the value never changes mid-call — long-running transactions
// (View, Update) hold their own reference for the txn duration.
func (s *Store) loadDB() *bolt.DB {
	return s.db.Load()
}

// Close closes the underlying bbolt database.
func (s *Store) Close() error {
	db := s.loadDB()
	if db == nil {
		return nil
	}
	return db.Close()
}

// Put encrypts value and writes it at bucket/key.
func (s *Store) Put(bucket, key string, value []byte) error {
	sealed, err := crypto.Seal(s.key, value)
	if err != nil {
		return fmt.Errorf("seal %s/%s: %w", bucket, key, err)
	}
	return s.loadDB().Update(func(tx *bolt.Tx) error {
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
	err := s.loadDB().View(func(tx *bolt.Tx) error {
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
	return s.loadDB().Update(func(tx *bolt.Tx) error {
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
	return s.loadDB().View(func(tx *bolt.Tx) error {
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
	return s.loadDB().View(func(tx *bolt.Tx) error {
		_, err := tx.WriteTo(w)
		return err
	})
}

// RestoreFromSnapshot replaces the underlying state.db with the
// bbolt dump in r and atomically swaps the live DB pointer. Outside
// references to *Store remain valid — only the inner *bolt.DB
// rotates. Returns nil on success.
//
// In-flight transactions on the old DB complete normally because
// they hold their own *bolt.DB reference for the duration. New
// transactions started after the swap see the restored content.
//
// Concurrent operations on this Store are safe except for the
// narrow window between Close(old) and Store(new) — a transaction
// started in that window would see the old (now-closed) handle.
// Raft serialises FSM.Restore calls so this can only race against
// non-FSM readers; the worst case is a transient ErrDatabaseNotOpen
// on those readers, which is far better than the pre-fix permanent
// failure across every component holding a stale Store pointer.
func (s *Store) RestoreFromSnapshot(r io.Reader) error {
	old := s.loadDB()
	if old == nil {
		return errors.New("store already closed")
	}
	path := old.Path()
	if err := old.Close(); err != nil {
		return fmt.Errorf("close state.db: %w", err)
	}
	tmp := path + ".restore.tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create tmp snapshot file: %w", err)
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write snapshot: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close tmp snapshot: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename snapshot into place: %w", err)
	}
	fresh, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return fmt.Errorf("reopen state.db: %w", err)
	}
	if err := fresh.Update(func(tx *bolt.Tx) error {
		for _, name := range allBuckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return fmt.Errorf("ensure bucket %q: %w", name, err)
			}
		}
		return nil
	}); err != nil {
		_ = fresh.Close()
		return err
	}
	s.db.Store(fresh)
	return nil
}
