package memory

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"

	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestStorePutGet(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	if err := s.Put(BucketPolicyRules, "rule-1", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(BucketPolicyRules, "rule-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("got %q, want hello", got)
	}
}

func TestStoreGetMissing(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	_, err := s.Get(BucketPolicyRules, "does-not-exist")
	if !errors.Is(err, types.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestStoreDeleteIdempotent(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	if err := s.Delete(BucketPolicyRules, "never-existed"); err != nil {
		t.Errorf("delete of absent key should not error: %v", err)
	}
	if err := s.Put(BucketPolicyRules, "x", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(BucketPolicyRules, "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(BucketPolicyRules, "x"); err != nil {
		t.Errorf("second delete should still not error: %v", err)
	}
}

func TestStoreEncryptsAtRest(t *testing.T) {
	t.Parallel()
	s, path := newTestStore(t)
	plaintext := []byte("this-should-never-appear-on-disk")
	if err := s.Put(BucketPolicyRules, "r", plaintext); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, plaintext) {
		t.Error("state.db contains plaintext — values not encrypted at rest")
	}
}

func TestStoreForEach(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	entries := map[string]string{
		"a": "one",
		"b": "two",
		"c": "three",
	}
	for k, v := range entries {
		if err := s.Put(BucketPolicyRules, k, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]string{}
	err := s.ForEach(BucketPolicyRules, func(key string, value []byte) error {
		seen[key] = string(value)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range entries {
		if seen[k] != v {
			t.Errorf("%s: got %q, want %q", k, seen[k], v)
		}
	}
}

func TestStoreSnapshotRestore(t *testing.T) {
	t.Parallel()
	s, path := newTestStore(t)
	if err := s.Put(BucketPolicyRules, "a", []byte("pre-snap")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := s.WriteSnapshot(&buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("snapshot is empty")
	}

	// Mutate after snapshot — restore should roll this back.
	if err := s.Put(BucketPolicyRules, "b", []byte("post-snap-will-be-gone")); err != nil {
		t.Fatal(err)
	}

	if err := s.RestoreFromSnapshot(&buf); err != nil {
		t.Fatalf("RestoreFromSnapshot: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	_ = path

	// Reads against the SAME *Store pointer must work post-restore —
	// this is the load-bearing invariant. Pre-fix, RestoreFromSnapshot
	// returned a NEW *Store and outside callers got "database not
	// open" on the original pointer. Now the Store rotates its inner
	// *bolt.DB in place via atomic.Pointer.
	got, err := s.Get(BucketPolicyRules, "a")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "pre-snap" {
		t.Errorf("after restore: got %q, want pre-snap", got)
	}
	_, err = s.Get(BucketPolicyRules, "b")
	if !errors.Is(err, types.ErrNotFound) {
		t.Errorf("b should be absent post-restore, err=%v", err)
	}
}

// TestRestoreFromSnapshotKeepsOutsideStorePointerValid is the
// regression test for the "database not open" production bug:
// callers (policy engine, scheduler, services) hold *Store
// references that were captured at boot. After raft applied a
// snapshot, those references must still produce reads against the
// fresh DB rather than against a closed handle.
func TestRestoreFromSnapshotKeepsOutsideStorePointerValid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	key, _ := crypto.GenerateKey()
	s, err := OpenStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Put(BucketPolicyRules, "before", []byte("v0")); err != nil {
		t.Fatal(err)
	}

	// Capture the same *Store pointer in a "remote consumer" — this
	// is the role policy.Engine, scheduler, services play in prod.
	consumer := s

	var snap bytes.Buffer
	if err := s.WriteSnapshot(&snap); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(BucketPolicyRules, "after-snap", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreFromSnapshot(&snap); err != nil {
		t.Fatal(err)
	}

	got, err := consumer.Get(BucketPolicyRules, "before")
	if err != nil {
		t.Fatalf("consumer.Get after restore: %v (the regression: pre-fix this was 'database not open')", err)
	}
	if string(got) != "v0" {
		t.Errorf("got %q, want v0", got)
	}
}

// ForEachKeys walks keys and opens nothing.
//
// Keys are the bucket's plaintext index. A caller that only needs them
// — the inbox drain reads recipient prefixes off them, on a
// thirty-second tick on every node — went through ForEach, which
// decrypts every value first. On a busy queue that is the whole inbox
// opened, repeatedly, to read a substring that was never encrypted.
//
// Asserted by planting bytes that cannot decrypt: if the walk still
// completes, it never looked at a value.
func TestForEachKeysNeverOpensAValue(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	for _, k := range []string{"devops:01A", "devops:01B", "engineering:01C"} {
		if err := store.Put(BucketBotInbox, k, []byte("payload")); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
	// A record this store's cipher cannot open.
	if err := store.putUnencrypted(BucketBotInbox, "devops:corrupt", []byte("not ciphertext")); err != nil {
		t.Fatalf("plant: %v", err)
	}

	var keys []string
	if err := store.ForEachKeys(BucketBotInbox, func(k string) error {
		keys = append(keys, k)
		return nil
	}); err != nil {
		t.Fatalf("ForEachKeys opened a value it did not need: %v", err)
	}
	if len(keys) != 4 {
		t.Errorf("walked %d keys, want 4: %v", len(keys), keys)
	}

	// And the contrast: ForEach, which does open them, fails here.
	// That is what makes the assertion above mean something.
	err := store.ForEach(BucketBotInbox, func(string, []byte) error { return nil })
	if err == nil {
		t.Error("ForEach opened an undecryptable record without complaint; " +
			"the keys-only walk is not being tested against anything")
	}
}

// putUnencrypted writes bytes straight into a bucket, bypassing the
// cipher — a record this store cannot open. Test-only, which is why it
// lives here rather than beside Put.
func (s *Store) putUnencrypted(bucket, key string, value []byte) error {
	return s.loadDB().Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("bucket %q not found", bucket)
		}
		return b.Put([]byte(key), value)
	})
}
