package memory

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	bolt "go.etcd.io/bbolt"
)

type failedSnapshotReader struct{ data []byte }

func (r *failedSnapshotReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, io.ErrUnexpectedEOF
}

func TestRestorePreparationFailurePreservesStore(t *testing.T) {
	for _, kind := range []string{"partial read", "empty", "invalid", "truncated"} {
		t.Run(kind, func(t *testing.T) {
			s, path := newTestStore(t)
			if err := s.Put(BucketPolicyRules, "original", []byte("retained")); err != nil {
				t.Fatal(err)
			}
			var snap bytes.Buffer
			if err := s.WriteSnapshot(&snap); err != nil {
				t.Fatal(err)
			}
			var reader io.Reader
			switch kind {
			case "partial read":
				reader = &failedSnapshotReader{data: snap.Bytes()[:4096]}
			case "empty":
				reader = bytes.NewReader(nil)
			case "invalid":
				reader = bytes.NewBufferString("not a database")
			case "truncated":
				reader = bytes.NewReader(snap.Bytes()[:len(snap.Bytes())/2])
			}
			if err := s.RestoreFromSnapshot(reader); err == nil {
				t.Fatal("invalid restore succeeded")
			}
			assertRestoreOriginal(t, s)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenStore(path, s.key)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			assertRestoreOriginal(t, reopened)
		})
	}
}

func assertRestoreOriginal(t *testing.T, s *Store) {
	t.Helper()
	got, err := s.Get(BucketPolicyRules, "original")
	if err != nil || string(got) != "retained" {
		t.Fatalf("original lost: %q %v", got, err)
	}
	if err := s.Put(BucketPolicyRules, "still-writable", []byte("yes")); err != nil {
		t.Fatal(err)
	}
	if err := s.ForEach(BucketPolicyRules, func(string, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteSnapshot(io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreCanRepeatAndSnapshotAgain(t *testing.T) {
	s, path := newTestStore(t)
	if err := s.Put(BucketPolicyRules, "original", []byte("retained")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		var snap bytes.Buffer
		if err := s.WriteSnapshot(&snap); err != nil {
			t.Fatal(err)
		}
		if err := s.Put(BucketPolicyRules, "extra", []byte("remove")); err != nil {
			t.Fatal(err)
		}
		if err := s.RestoreFromSnapshot(&snap); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(BucketPolicyRules, "extra"); !IsNotFound(err) {
			t.Fatalf("extra survived: %v", err)
		}
		assertRestoreOriginal(t, s)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, s.key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	assertRestoreOriginal(t, reopened)
}

func TestRestoreAfterCloseDoesNotResurrectStore(t *testing.T) {
	s, _ := newTestStore(t)
	var snap bytes.Buffer
	if err := s.WriteSnapshot(&snap); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreFromSnapshot(&snap); err == nil {
		t.Fatal("restored closed store")
	}
}

type failingSnapshotFile struct {
	snapshotFile
	stage string
	err   error
}

func (f failingSnapshotFile) Write(p []byte) (int, error) {
	if f.stage == "write" {
		return 0, f.err
	}
	return f.snapshotFile.Write(p)
}
func (f failingSnapshotFile) Sync() error {
	if f.stage == "sync" {
		return f.err
	}
	return f.snapshotFile.Sync()
}
func (f failingSnapshotFile) Close() error {
	err := f.snapshotFile.Close()
	if f.stage == "close" {
		return errors.Join(err, f.err)
	}
	return err
}

func TestRestoreIOFailuresPreserveOriginal(t *testing.T) {
	for _, stage := range []string{"create", "write", "sync", "close", "prepare", "link", "recovery sync", "rename", "install sync"} {
		t.Run(stage, func(t *testing.T) {
			s, path := newTestStore(t)
			var snap bytes.Buffer
			if err := s.WriteSnapshot(&snap); err != nil {
				t.Fatal(err)
			}
			if err := s.Put(BucketPolicyRules, "original", []byte("retained")); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected " + stage)
			ops := defaultSnapshotRestoreOps()
			switch stage {
			case "create":
				ops.create = func(string, string) (snapshotFile, error) { return nil, injected }
			case "write", "sync", "close":
				create := ops.create
				ops.create = func(dir, pattern string) (snapshotFile, error) {
					f, err := create(dir, pattern)
					if err != nil {
						return nil, err
					}
					return failingSnapshotFile{f, stage, injected}, nil
				}
			case "prepare":
				ops.prepare = func(string) (*bolt.DB, error) { return nil, injected }
			case "link":
				ops.link = func(string, string) error { return injected }
			case "rename":
				ops.rename = func(string, string) error { return injected }
			case "recovery sync", "install sync":
				count := 0
				failAt := 1
				if stage == "install sync" {
					failAt = 2
				}
				ops.syncDir = func(path string) error {
					count++
					if count == failAt {
						return injected
					}
					return syncSnapshotDirectory(path)
				}
			}
			out, err := s.restoreSnapshot(&snap, ops)
			if !errors.Is(err, injected) || out.committed {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			assertRestoreOriginal(t, s)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenStore(path, s.key)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			assertRestoreOriginal(t, reopened)
			files, err := os.ReadDir(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				if strings.Contains(f.Name(), ".restore-") {
					t.Fatalf("leaked staging file %s", f.Name())
				}
			}
		})
	}
}

func TestRestoreFailedRollbackDisablesStoreAndPreservesRecovery(t *testing.T) {
	for _, stage := range []string{"candidate link", "rollback link", "rollback rename", "rollback sync"} {
		t.Run(stage, func(t *testing.T) {
			s, path := newTestStore(t)
			if err := s.Put(BucketPolicyRules, "original", []byte("retained")); err != nil {
				t.Fatal(err)
			}
			var snap bytes.Buffer
			if err := s.WriteSnapshot(&snap); err != nil {
				t.Fatal(err)
			}
			ops := defaultSnapshotRestoreOps()
			calls := 0
			ops.syncDir = func(dir string) error {
				calls++
				if calls == 2 || (stage == "rollback sync" && calls == 3) {
					return errors.New("sync failed")
				}
				return syncSnapshotDirectory(dir)
			}
			links := 0
			ops.link = func(a, b string) error {
				links++
				if (stage == "candidate link" && links == 2) || (stage == "rollback link" && links == 3) {
					return errors.New("link failed")
				}
				return os.Link(a, b)
			}
			renames := 0
			ops.rename = func(a, b string) error {
				renames++
				if stage == "rollback rename" && renames == 2 {
					return errors.New("rename failed")
				}
				return os.Rename(a, b)
			}
			out, err := s.restoreSnapshot(&snap, ops)
			if !errors.Is(err, ErrRestoreRecoveryRequired) || out.committed {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			select {
			case <-s.Failed():
			default:
				t.Fatal("no terminal failure signal")
			}
			if !errors.Is(s.Failure(), ErrRestoreRecoveryRequired) {
				t.Fatal(s.Failure())
			}
			if err := s.Put(BucketPolicyRules, "bad", []byte("must fail")); err == nil {
				t.Fatal("poisoned store accepted write")
			}
			if err := s.RestoreFromSnapshot(bytes.NewReader(nil)); !errors.Is(err, ErrRestoreRecoveryRequired) {
				t.Fatal(err)
			}
			if _, err := OpenStore(path, s.key); !errors.Is(err, ErrRestoreRecoveryRequired) {
				t.Fatalf("restart ignored recovery files: %v", err)
			}
			previous, err := filepath.Glob(path + ".restore-*.previous")
			if err != nil || len(previous) != 1 {
				t.Fatalf("missing old copy: %v %v", previous, err)
			}
			// Raw open for operator inspection: the old file is still a valid DB.
			old, err := bolt.Open(previous[0], 0o600, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := old.View(func(tx *bolt.Tx) error {
				if tx.Bucket([]byte(BucketPolicyRules)).Get([]byte("original")) == nil {
					return errors.New("old record lost")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			_ = old.Close()
			candidate, err := os.Stat(path)
			if err != nil || candidate.Size() == 0 {
				t.Fatalf("canonical image lost: %v", err)
			}
		})
	}
}

func TestRestoreCleanupFailureIsCommitted(t *testing.T) {
	for _, stage := range []string{"old close", "backup removal", "directory sync"} {
		t.Run(stage, func(t *testing.T) {
			s, _ := newTestStore(t)
			if err := s.Put(BucketPolicyRules, "original", []byte("retained")); err != nil {
				t.Fatal(err)
			}
			var snap bytes.Buffer
			if err := s.WriteSnapshot(&snap); err != nil {
				t.Fatal(err)
			}
			if err := s.Put(BucketPolicyRules, "extra", []byte("remove")); err != nil {
				t.Fatal(err)
			}
			ops := defaultSnapshotRestoreOps()
			switch stage {
			case "old close":
				ops.closeOld = func(db *bolt.DB) error { return errors.Join(db.Close(), errors.New("close failed")) }
			case "backup removal":
				ops.remove = func(path string) error {
					if strings.HasSuffix(path, ".previous") {
						return errors.New("remove failed")
					}
					return os.Remove(path)
				}
			case "directory sync":
				count := 0
				ops.syncDir = func(path string) error {
					count++
					if count == 3 {
						return errors.New("cleanup sync failed")
					}
					return syncSnapshotDirectory(path)
				}
			}
			out, err := s.restoreSnapshot(&snap, ops)
			if err != nil || !out.committed || out.warning == nil {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			assertRestoreOriginal(t, s)
			if _, err := s.Get(BucketPolicyRules, "extra"); !IsNotFound(err) {
				t.Fatalf("old state survived: %v", err)
			}

			if stage == "old close" {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := OpenStore(s.path, s.key)
				if err != nil {
					t.Fatalf("committed restore cannot reopen: %v", err)
				}
				defer func() { _ = reopened.Close() }()
				assertRestoreOriginal(t, reopened)
			}
		})
	}
}

func TestRestoreSerializesCloseAndDrainsTransactions(t *testing.T) {
	s, _ := newTestStore(t)
	var snap bytes.Buffer
	if err := s.WriteSnapshot(&snap); err != nil {
		t.Fatal(err)
	}
	tx, err := s.loadDB().Begin(false)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	ops := defaultSnapshotRestoreOps()
	oldClose := ops.closeOld
	ops.closeOld = func(db *bolt.DB) error { close(entered); <-release; return oldClose(db) }
	done := make(chan error, 1)
	go func() { _, err := s.restoreSnapshot(&snap, ops); done <- err }()
	<-entered
	// The fresh handle is already published while the old transaction drains.
	if err := s.Put(BucketPolicyRules, "new", []byte("works")); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	close(release)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreFromSnapshot(bytes.NewReader(nil)); err == nil {
		t.Fatal("close was undone")
	}
}

func TestFSMRestoreRefreshesLastApplied(t *testing.T) {
	s, _ := newTestStore(t)
	f := NewFSM(s)
	set := func(index uint64) {
		raw := make([]byte, 8)
		binary.BigEndian.PutUint64(raw, index)
		if err := s.Put(BucketRaftMeta, KeyLastAppliedIndex, raw); err != nil {
			t.Fatal(err)
		}
	}
	set(7)
	var snap bytes.Buffer
	if err := s.WriteSnapshot(&snap); err != nil {
		t.Fatal(err)
	}
	set(12)
	if f.lastApplied() != 12 {
		t.Fatal("fixture index")
	}
	if err := f.Restore(io.NopCloser(&snap)); err != nil {
		t.Fatal(err)
	}
	if f.lastApplied() != 7 {
		t.Fatal("stale index after restore")
	}
}

func TestRestoreFatalFailureStopsRaft(t *testing.T) {
	node, fsm := newTestRaft(t)
	var snapshot bytes.Buffer
	if err := fsm.store.WriteSnapshot(&snapshot); err != nil {
		t.Fatal(err)
	}
	ops := defaultSnapshotRestoreOps()
	calls := 0
	ops.syncDir = func(string) error {
		calls++
		if calls >= 2 {
			return errors.New("injected disk failure")
		}
		return nil
	}
	if _, err := fsm.store.restoreSnapshot(bytes.NewReader(snapshot.Bytes()), ops); !errors.Is(err, ErrRestoreRecoveryRequired) {
		t.Fatalf("restore: %v", err)
	}
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for node.Raft.State() != raft.Shutdown {
		select {
		case <-deadline:
			t.Fatal("Raft continued after terminal store failure")
		case <-ticker.C:
		}
	}
}

func TestRecoveryInspectionIsReadOnly(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Put(BucketPolicyRules, "original", []byte("retained")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	marker := s.path + ".restore-inspect.previous"
	if err := os.Link(s.path, marker); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(s.path, s.key); !errors.Is(err, ErrRestoreRecoveryRequired) {
		t.Fatalf("write open: %v", err)
	}
	ro, err := OpenStoreReadOnly(s.path, s.key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	if got, err := ro.Get(BucketPolicyRules, "original"); err != nil || string(got) != "retained" {
		t.Fatalf("read: %q %v", got, err)
	}
	if err := ro.Put(BucketPolicyRules, "changed", []byte("no")); err == nil {
		t.Fatal("inspection allowed write")
	}
	if err := ro.RestoreFromSnapshot(bytes.NewReader(nil)); err == nil {
		t.Fatal("inspection allowed restore")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("inspection removed recovery marker")
	}
	if _, err := OpenStoreReadOnly(s.path+".missing", s.key); err == nil {
		t.Fatal("inspection created missing database")
	}
}
