// Package backup stores independent encrypted archive generations locally.
package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"filippo.io/age"

	"github.com/jmylchreest/lobslaw/internal/archive"
)

type Repository struct {
	Path string
}

type Generation struct {
	Manifest archive.Manifest `json:"manifest"`
	SHA256   string           `json:"sha256"`
	Pinned   bool             `json:"pinned"`
}

type Retention struct {
	KeepLast   int
	KeepWithin time.Duration
}

// A crash leaves the lock directory behind. Recovery must check that no writer
// remains before removing it; an old lock is not evidence that its owner died.
func (r Repository) lock() (func(), error) {
	if r.Path == "" {
		return nil, errors.New("backup repository path required")
	}
	if err := os.MkdirAll(r.Path, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(r.Path, ".lock")
	if err := os.Mkdir(path, 0o700); err != nil {
		return nil, fmt.Errorf("backup repository locked; after a crash, check for an active writer before removing .lock: %w", err)
	}
	return func() { _ = os.Remove(path) }, nil
}

func validID(id string) bool {
	if len(id) == 0 || len(id) > maxGenerationIDBytes {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (r Repository) Create(snapshot archive.Snapshot, recipients ...age.Recipient) (Generation, error) {
	var generation Generation
	if len(recipients) == 0 || !validID(snapshot.Manifest.SnapshotID) {
		return generation, errors.New("backup requires encryption recipients and a safe snapshot id")
	}
	// Verify the logical encoding before encrypting. Plaintext never reaches disk.
	var plain bytes.Buffer
	if err := archive.Write(&plain, snapshot); err != nil {
		return generation, err
	}
	verified, err := archive.Read(&plain)
	if err != nil {
		return generation, err
	}
	unlock, err := r.lock()
	if err != nil {
		return generation, err
	}
	defer unlock()
	dir := filepath.Join(r.Path, snapshot.Manifest.SnapshotID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return generation, fmt.Errorf("create immutable generation: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(dir, "archive.age"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return generation, err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if err := archive.Write(io.MultiWriter(file, hash), verified, recipients...); err != nil {
		return generation, err
	}
	if err := file.Sync(); err != nil {
		return generation, err
	}
	if err := file.Close(); err != nil {
		return generation, err
	}
	generation = Generation{Manifest: verified.Manifest, SHA256: hex.EncodeToString(hash.Sum(nil))}
	raw, err := json.Marshal(generation)
	if err != nil {
		return generation, err
	}
	if err := writeNewFile(filepath.Join(dir, ".manifest"), raw); err != nil {
		return generation, err
	}
	// Publish the completion marker last. Interrupted generations remain invisible.
	if err := os.Rename(filepath.Join(dir, ".manifest"), filepath.Join(dir, "manifest.json")); err != nil {
		return generation, err
	}
	if err := syncDirectory(dir); err != nil {
		return generation, err
	}
	return generation, syncDirectory(r.Path)
}

func writeNewFile(path string, raw []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(raw); err != nil {
		return err
	}
	return file.Sync()
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func (r Repository) List() ([]Generation, error) {
	unlock, err := r.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	entries, err := os.ReadDir(r.Path)
	if err != nil {
		return nil, err
	}
	var out []Generation
	for _, entry := range entries {
		if !entry.IsDir() || !validID(entry.Name()) {
			continue
		}
		generation, err := r.generation(entry.Name())
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, generation)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Manifest.CreatedAt.Equal(out[j].Manifest.CreatedAt) {
			return out[i].Manifest.SnapshotID > out[j].Manifest.SnapshotID
		}
		return out[i].Manifest.CreatedAt.After(out[j].Manifest.CreatedAt)
	})
	return out, nil
}

func regularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("backup contains a non-regular file")
	}
	return nil
}

func (r Repository) generation(id string) (Generation, error) {
	var generation Generation
	if !validID(id) {
		return generation, errors.New("invalid backup snapshot id")
	}
	dir := filepath.Join(r.Path, id)
	info, err := os.Lstat(dir)
	if err != nil {
		return generation, err
	}
	if !info.IsDir() {
		return generation, errors.New("backup generation is not a directory")
	}
	path := filepath.Join(dir, "manifest.json")
	if err := regularFile(path); err != nil {
		return generation, err
	}
	file, err := os.Open(path)
	if err != nil {
		return generation, err
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxManifestBytes))
	err = decoder.Decode(&generation)
	_ = file.Close()
	if err != nil {
		return generation, err
	}
	if generation.Manifest.SnapshotID != id || generation.Manifest.Format != archive.Format || !archive.SupportsSchema(generation.Manifest.SchemaVersion) {
		return generation, errors.New("invalid backup generation manifest")
	}
	path = filepath.Join(dir, "archive.age")
	if err := regularFile(path); err != nil {
		return generation, fmt.Errorf("completed backup payload: %v", err)
	}
	file, err = os.Open(path)
	if err != nil {
		return generation, err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, archive.MaxArchiveBytes+1))
	_ = file.Close()
	if err != nil {
		return generation, err
	}
	if size > archive.MaxArchiveBytes || hex.EncodeToString(hash.Sum(nil)) != generation.SHA256 {
		return generation, fmt.Errorf("backup %s failed ciphertext checksum verification", id)
	}
	err = regularFile(filepath.Join(dir, "pinned"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return generation, err
	}
	generation.Pinned = err == nil
	return generation, nil
}

func (r Repository) Read(id string, identities ...age.Identity) (archive.Snapshot, error) {
	unlock, err := r.lock()
	if err != nil {
		return archive.Snapshot{}, err
	}
	defer unlock()
	generation, err := r.generation(id)
	if err != nil {
		return archive.Snapshot{}, err
	}
	file, err := os.Open(filepath.Join(r.Path, id, "archive.age"))
	if err != nil {
		return archive.Snapshot{}, err
	}
	defer func() { _ = file.Close() }()
	snapshot, err := archive.Read(file, identities...)
	if err != nil {
		return archive.Snapshot{}, err
	}
	if snapshot.Manifest.SnapshotID != generation.Manifest.SnapshotID || !snapshot.Manifest.CreatedAt.Equal(generation.Manifest.CreatedAt) {
		return archive.Snapshot{}, errors.New("backup manifest disagrees with authenticated archive")
	}
	return snapshot, nil
}

func (r Repository) Pin(id string, pinned bool) error {
	unlock, err := r.lock()
	if err != nil {
		return err
	}
	defer unlock()
	generation, err := r.generation(id)
	if err != nil {
		return err
	}
	if generation.Pinned == pinned {
		return nil
	}
	path := filepath.Join(r.Path, id, "pinned")
	if pinned {
		err = writeNewFile(path, []byte("pinned\n"))
	} else {
		err = os.Remove(path)
	}
	if err != nil {
		return err
	}
	return syncDirectory(filepath.Join(r.Path, id))
}

func (r Repository) PlanPrune(retention Retention, now time.Time) ([]Generation, error) {
	if retention.KeepLast < 0 || retention.KeepWithin < 0 || (retention.KeepLast == 0 && retention.KeepWithin == 0) {
		return nil, errors.New("retention requires a positive count or age")
	}
	generations, err := r.List()
	if err != nil {
		return nil, err
	}
	var removed []Generation
	for i, generation := range generations {
		if generation.Pinned || i < retention.KeepLast {
			continue
		}
		if retention.KeepWithin > 0 && !generation.Manifest.CreatedAt.Before(now.Add(-retention.KeepWithin)) {
			continue
		}
		removed = append(removed, generation)
	}
	return removed, nil
}

func (r Repository) Prune(plan []Generation) error {
	unlock, err := r.lock()
	if err != nil {
		return err
	}
	defer unlock()
	// Validate every candidate under the pinning lock before the first deletion.
	for _, expected := range plan {
		current, err := r.generation(expected.Manifest.SnapshotID)
		if err != nil {
			return err
		}
		if current.Pinned || current.SHA256 != expected.SHA256 {
			return errors.New("backup prune plan changed; re-plan")
		}
		entries, err := os.ReadDir(filepath.Join(r.Path, expected.Manifest.SnapshotID))
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Name() != "manifest.json" && entry.Name() != "archive.age" {
				return errors.New("backup contains unexpected files; refusing prune")
			}
		}
	}
	for _, generation := range plan {
		dir := filepath.Join(r.Path, generation.Manifest.SnapshotID)
		for _, name := range []string{"manifest.json", "archive.age"} {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return err
			}
		}
		if err := os.Remove(dir); err != nil {
			return err
		}
	}
	return syncDirectory(r.Path)
}
