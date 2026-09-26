package memory

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// ErrRestoreRecoveryRequired means automatic rollback could not establish a
// durable canonical file. Recovery copies must be inspected before restarting.
var ErrRestoreRecoveryRequired = errors.New("snapshot restore requires operator recovery")

type restoreFailure struct{ err error }

// Failed is closed when a restore cannot safely roll back. Nil stores disable
// the corresponding select case on nodes without a memory function.
func (s *Store) Failed() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.failed
}

// Failure returns the terminal restore error, if any.
func (s *Store) Failure() error {
	if failure := s.failure.Load(); failure != nil {
		return failure.err
	}
	return nil
}

type snapshotFile interface {
	io.Writer
	Name() string
	Sync() error
	Close() error
}

// Dependencies belong to one restore invocation, never package-level mutable
// hooks. Tests can fail actual I/O boundaries without filling a disk.
type snapshotRestoreOps struct {
	create   func(string, string) (snapshotFile, error)
	prepare  func(string) (*bolt.DB, error)
	link     func(string, string) error
	rename   func(string, string) error
	remove   func(string) error
	syncDir  func(string) error
	closeOld func(*bolt.DB) error
}

func defaultSnapshotRestoreOps() snapshotRestoreOps {
	return snapshotRestoreOps{
		create:  func(dir, pattern string) (snapshotFile, error) { return os.CreateTemp(dir, pattern) },
		prepare: prepareSnapshotDB, link: os.Link, rename: os.Rename, remove: os.Remove,
		syncDir: syncSnapshotDirectory, closeOld: func(db *bolt.DB) error { return db.Close() },
	}
}

type snapshotRestoreOutcome struct {
	committed bool
	warning   error
}

// RestoreFromSnapshot prepares and validates a new database while the old one
// remains usable. The canonical filename and live pointer change only after a
// candidate is ready. Outside Store pointers remain valid. A reader that loaded
// the old DB just before publication may still see ErrDatabaseNotOpen when it
// starts a transaction after the old handle closes; existing transactions drain.
func (s *Store) RestoreFromSnapshot(r io.Reader) error {
	outcome, err := s.restoreSnapshot(r, defaultSnapshotRestoreOps())
	if outcome.warning != nil {
		// A committed restore must return success so FSM.Restore invalidates its
		// last-applied cache and Raft advances its snapshot metadata consistently.
		slog.Warn("snapshot restore cleanup incomplete", "committed", outcome.committed, "path", s.path, "err", outcome.warning)
	}
	return err
}

func (s *Store) restoreSnapshot(r io.Reader, ops snapshotRestoreOps) (out snapshotRestoreOutcome, err error) {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if failure := s.Failure(); failure != nil {
		return out, failure
	}
	if s.readOnly {
		return out, errors.New("cannot restore a read-only store")
	}
	if s.closed {
		return out, errors.New("store already closed")
	}
	old := s.loadDB()
	dir := filepath.Dir(s.path)
	file, err := ops.create(dir, filepath.Base(s.path)+".restore-*")
	if err != nil {
		return out, fmt.Errorf("create snapshot: %w", err)
	}
	tmp := file.Name()
	preserve := false
	defer func() {
		if !preserve {
			if e := ops.remove(tmp); e != nil && !os.IsNotExist(e) {
				out.warning = errors.Join(out.warning, e)
			}
		}
	}()
	n, copyErr := io.Copy(file, r)
	if copyErr == nil {
		copyErr = file.Sync()
	}
	closeErr := file.Close()
	if err = errors.Join(copyErr, closeErr); err != nil {
		return out, fmt.Errorf("write snapshot: %w", err)
	}
	if n == 0 {
		return out, errors.New("empty snapshot")
	}
	fresh, err := ops.prepare(tmp)
	if err != nil {
		return out, fmt.Errorf("prepare snapshot: %w", err)
	}
	defer func() {
		if !out.committed {
			_ = fresh.Close()
		}
	}()
	backup := tmp + ".previous"
	if err := ops.link(s.path, backup); err != nil {
		return out, fmt.Errorf("preserve current snapshot: %w", err)
	}
	// Keep the recovery link on ambiguous failures and cleanup warnings.
	removeBackup := func() {
		if e := ops.remove(backup); e != nil {
			out.warning = errors.Join(out.warning, e)
		}
	}
	if err := ops.syncDir(dir); err != nil {
		removeBackup()
		return out, fmt.Errorf("sync recovery link: %w", err)
	}
	if err := ops.rename(tmp, s.path); err != nil {
		removeBackup()
		return out, fmt.Errorf("install snapshot: %w", err)
	}
	if err := ops.syncDir(dir); err != nil {
		rollbackErr := rollbackSnapshot(ops, s.path, tmp, backup)
		if rollbackErr != nil {
			preserve = true
			fatal := fmt.Errorf("%w (current %s, previous %s, candidate %s): %w", ErrRestoreRecoveryRequired, s.path, backup, tmp, errors.Join(err, rollbackErr))
			s.failure.Store(&restoreFailure{err: fatal})
			s.closed = true
			// Disable all existing Store users, not just the restore caller. Signal
			// before draining transactions so the owning node starts shutting down.
			close(s.failed)
			_ = old.Close()
			return out, fatal
		}
		removeBackup()
		return out, fmt.Errorf("sync installed snapshot (rolled back): %w", err)
	}
	s.db.Store(fresh)
	out.committed = true
	if err := ops.closeOld(old); err != nil {
		out.warning = fmt.Errorf("close previous database: %w", err)
	}
	removeBackup()
	out.warning = errors.Join(out.warning, ops.syncDir(dir))
	return out, nil
}

func rollbackSnapshot(ops snapshotRestoreOps, path, tmp, backup string) error {
	// Retain both images until rollback is durable. Never consume the recovery
	// link itself: a second I/O failure must not erase the evidence for recovery.
	if err := ops.link(path, tmp); err != nil {
		return fmt.Errorf("retain candidate: %w", err)
	}
	rollback := tmp + ".rollback"
	if err := ops.link(backup, rollback); err != nil {
		return fmt.Errorf("link rollback: %w", err)
	}
	if err := ops.rename(rollback, path); err != nil {
		return fmt.Errorf("restore previous path: %w", err)
	}
	return ops.syncDir(filepath.Dir(path))
}

func prepareSnapshotDB(path string) (*bolt.DB, error) {
	// Read-only Open avoids loading a corrupt freelist before checking the
	// snapshot's declared size against its real length.
	check, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	err = check.View(func(tx *bolt.Tx) error {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if tx.Size() > info.Size() {
			return errors.New("truncated snapshot")
		}
		var problems error
		for err := range tx.Check() {
			problems = errors.Join(problems, err)
		}
		return problems
	})
	err = errors.Join(err, check.Close())
	if err != nil {
		return nil, err
	}
	fresh, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	err = fresh.Update(func(tx *bolt.Tx) error {
		for _, name := range allBuckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		err = fresh.Sync()
	}
	if err != nil {
		_ = fresh.Close()
		return nil, err
	}
	return fresh, nil
}

func syncSnapshotDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func checkRestoreRecovery(path string) error {
	// No glob patterns: database filenames may themselves contain metacharacters.
	entries, err := os.ReadDir(filepath.Dir(path))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, filepath.Base(path)+".restore-") && strings.HasSuffix(name, ".previous") {
			return fmt.Errorf("%w: inspect %s before opening %s", ErrRestoreRecoveryRequired, filepath.Join(filepath.Dir(path), name), path)
		}
	}
	return nil
}
