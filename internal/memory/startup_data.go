package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	bolt "go.etcd.io/bbolt"

	"github.com/jmylchreest/lobslaw/internal/dataformat"
	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
)

const startupBackupMarker = "startup-backup.json"

type StartupDataStatus struct {
	Before   StateFormat `json:"before"`
	After    StateFormat `json:"after"`
	Backup   string      `json:"backup,omitempty"`
	Migrated bool        `json:"migrated"`
}

// OpenNodeStore validates node logs and snapshots before the state adapter can
// migrate or rebuild anything. Existing DB locks cover validation and backup.
func OpenNodeStore(dir string, key crypto.Key) (*Store, StartupDataStatus, error) {
	var report StartupDataStatus
	stateMissing := false
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() {
			return nil, report, errors.New("node data must be a directory, not a symlink")
		}
	} else if !os.IsNotExist(err) {
		return nil, report, err
	}
	if _, err := os.Lstat(filepath.Join(dir, "state.db")); err == nil {
		if err := regularDataPath(filepath.Join(dir, "state.db")); err != nil {
			return nil, report, err
		}
	} else if !os.IsNotExist(err) {
		return nil, report, err
	} else {
		stateMissing = true
	}
	if _, err := os.Lstat(filepath.Join(dir, startupBackupMarker)); err == nil {
		return nil, report, errors.New("startup backup is a rollback image, not a live node; inspect or migrate it explicitly before recovery")
	} else if !os.IsNotExist(err) {
		return nil, report, err
	}
	manifest, err := dataformat.ReadManifest(dir)
	if err != nil {
		return nil, report, err
	}
	if stateMissing {
		// A snapshot-backed restart may legitimately lack state.db. Check
		// retained artifacts first so incompatible input cannot create one.
		probe := &Store{key: key}
		logs, err := probe.openNodeLogs(dir)
		if err != nil {
			return nil, report, err
		}
		artifactErr := probe.checkNodeArtifacts(dir, manifest, logs)
		if logs != nil {
			_ = logs.Close()
		}
		if artifactErr != nil {
			return nil, report, artifactErr
		}
	}
	store, err := openStore(filepath.Join(dir, "state.db"), key, false, func(store *Store) error {
		return store.preflightNodeData(dir, manifest, &report)
	})
	if err != nil {
		return nil, report, err
	}
	err = store.loadDB().View(func(tx *bolt.Tx) error {
		var err error
		report.After, err = readStateFormat(tx)
		return err
	})
	if err != nil {
		_ = store.Close()
		return nil, report, err
	}
	return store, report, nil
}

func (s *Store) preflightNodeData(dir string, manifest dataformat.Manifest, report *StartupDataStatus) error {
	var populated bool
	if err := s.loadDB().View(func(tx *bolt.Tx) error {
		var err error
		report.Before, err = validateStateFormat(tx, s.cipher, false)
		if err != nil {
			return err
		}
		return tx.ForEach(func(_ []byte, bucket *bolt.Bucket) error {
			if key, _ := bucket.Cursor().First(); key != nil {
				populated = true
			}
			return nil
		})
	}); err != nil {
		return fmt.Errorf("startup state compatibility: %w", err)
	}
	logs, err := s.openNodeLogs(dir)
	if err != nil {
		return err
	}
	if logs != nil {
		defer func() { _ = logs.Close() }()
	}
	if err := s.checkNodeArtifacts(dir, manifest, logs); err != nil {
		return err
	}
	if report.Before.Version == 0 && populated {
		if logs == nil {
			return errors.New("populated legacy state has no raft.db; restore the complete node data before automatic startup migration")
		}
		report.Migrated = true
		report.Backup, err = s.backupBeforeStartupMigration(dir, report.Before, manifest.LegacyFormat, logs != nil)
		if err != nil {
			return fmt.Errorf("backup before startup migration: %w", err)
		}
	}
	return nil
}

func (s *Store) openNodeLogs(dir string) (*raftboltdb.BoltStore, error) {
	var logs *raftboltdb.BoltStore
	raftPath := filepath.Join(dir, RaftLogFile)
	if _, err := os.Lstat(raftPath); err == nil {
		if err := regularDataPath(raftPath); err != nil {
			return nil, err
		}
		var err error
		logs, err = raftboltdb.New(raftboltdb.Options{Path: raftPath, BoltOptions: &bolt.Options{ReadOnly: true, Timeout: storeOpenTimeout}})
		if err != nil {
			return nil, fmt.Errorf("startup Raft database locked or unreadable: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return logs, nil
}

func (s *Store) checkNodeArtifacts(dir string, manifest dataformat.Manifest, logs *raftboltdb.BoltStore) error {
	// The snapshot validator uses private copies; it never passes the live
	// repository to the library's write-on-open constructor.
	_, covered, err := inspectSnapshots(context.Background(), dir, s.key)
	if err != nil {
		return fmt.Errorf("startup snapshot compatibility: %w", err)
	}
	if logs != nil {
		if _, err := preflightLogs(context.Background(), logs, manifest.LegacyFormat, covered); err != nil {
			return fmt.Errorf("startup log compatibility: %w; if the legacy layout is ambiguous, use lobslaw data inspect/migrate with verified --legacy-format provenance", err)
		}
	}
	return nil
}

// Explicit paths avoid recursive copies of old backups or large model caches.
// The state write lock and Raft read lock exclude running node writers.
func (s *Store) backupBeforeStartupMigration(dir string, before StateFormat, profile string, hasRaft bool) (string, error) {
	stage, err := os.MkdirTemp(dir, ".startup-backup-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	// The store has not been published and its exclusive bbolt lock is held;
	// copying the raw file preserves even its original metadata pages.
	if err := copyDataTree(context.Background(), s.path, filepath.Join(stage, "state.db")); err != nil {
		return "", err
	}
	paths := []string{SnapshotDir}
	if hasRaft {
		paths = append(paths, RaftLogFile)
	}
	if _, err := os.Lstat(filepath.Join(dir, dataformat.ManifestName)); err == nil {
		paths = append(paths, dataformat.ManifestName)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	for _, path := range paths {
		if err := copyDataTree(context.Background(), filepath.Join(dir, path), filepath.Join(stage, path)); err != nil {
			return "", err
		}
	}
	marker, err := json.Marshal(struct {
		Version      int         `json:"version"`
		State        StateFormat `json:"state"`
		LegacyFormat string      `json:"legacy_format,omitempty"`
	}{Version: 1, State: before, LegacyFormat: profile})
	if err != nil {
		return "", err
	}
	if err := writeDataFile(filepath.Join(stage, startupBackupMarker), bytes.NewReader(marker)); err != nil {
		return "", err
	}
	if err := syncSnapshotDirectory(stage); err != nil {
		return "", err
	}
	destination := filepath.Join(dir, "startup-backup-"+ids.New())
	if err := os.Rename(stage, destination); err != nil {
		return "", err
	}
	if err := syncSnapshotDirectory(dir); err != nil {
		return "", fmt.Errorf("backup published at %s but directory sync failed: %w", destination, err)
	}
	return destination, nil
}
