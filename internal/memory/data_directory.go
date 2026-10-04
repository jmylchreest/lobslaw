package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	bolt "go.etcd.io/bbolt"

	"github.com/jmylchreest/lobslaw/internal/dataformat"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
)

// DataInspection contains format metadata only: no record content or secrets.
type DataInspection struct {
	State           StateFormat `json:"state"`
	LegacyFormat    string      `json:"legacy_format,omitempty"`
	FirstLog        uint64      `json:"first_log"`
	LastLog         uint64      `json:"last_log"`
	Snapshots       int         `json:"snapshots"`
	Protocol        string      `json:"protocol"`
	RestoreRequired bool        `json:"restore_required"`
}

type dataSource struct {
	state    *bolt.DB
	logs     *raftboltdb.BoltStore
	manifest dataformat.Manifest
	report   DataInspection
}

func (s *dataSource) close() {
	if s.logs != nil {
		_ = s.logs.Close()
	}
	if s.state != nil {
		_ = s.state.Close()
	}
}

func regularDataPath(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("data file %s is not regular", path)
	}
	return nil
}

// Both bbolt locks remain held for the entire inspection/copy. This is offline:
// it cannot race a live writer or create a missing source database.
func openDataSource(ctx context.Context, dir, profile string, key crypto.Key) (*dataSource, error) {
	s := &dataSource{}
	ok := false
	defer func() {
		if !ok {
			s.close()
		}
	}()
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("source must be a directory, not a symlink")
	}
	if err := checkRestoreRecovery(filepath.Join(dir, "state.db")); err != nil {
		return nil, err
	}
	s.manifest, err = dataformat.ReadManifest(dir)
	if err != nil {
		return nil, err
	}
	if !dataformat.ValidLegacy(profile) {
		return nil, errors.New("unknown legacy format")
	}
	if s.manifest.LegacyFormat != "" && profile != "" && profile != s.manifest.LegacyFormat {
		return nil, errors.New("legacy format contradicts existing provenance")
	}
	if profile == "" {
		profile = s.manifest.LegacyFormat
	}
	for _, name := range []string{"state.db", RaftLogFile} {
		if err := regularDataPath(filepath.Join(dir, name)); err != nil {
			return nil, err
		}
	}
	s.state, err = bolt.Open(filepath.Join(dir, "state.db"), 0o600, &bolt.Options{ReadOnly: true, Timeout: storeOpenTimeout})
	if err != nil {
		return nil, fmt.Errorf("source state locked or unreadable (stop node): %w", err)
	}
	s.logs, err = raftboltdb.New(raftboltdb.Options{Path: filepath.Join(dir, RaftLogFile), BoltOptions: &bolt.Options{ReadOnly: true, Timeout: storeOpenTimeout}})
	if err != nil {
		return nil, err
	}
	cipher, err := crypto.NewCipher(key)
	if err != nil {
		return nil, err
	}
	err = s.state.View(func(tx *bolt.Tx) error {
		var err error
		s.report.State, err = validateStateFormat(tx, cipher, true)
		if err != nil {
			return err
		}
		var problems error
		for err := range tx.Check() {
			problems = errors.Join(problems, err)
		}
		return problems
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var covered uint64
	s.report.Snapshots, covered, err = inspectSnapshots(ctx, dir, key)
	if err != nil {
		return nil, err
	}
	if _, err := preflightLogs(ctx, s.logs, profile, covered); err != nil {
		return nil, err
	}
	s.report.FirstLog, err = s.logs.FirstIndex()
	if err != nil {
		return nil, err
	}
	s.report.LastLog, err = s.logs.LastIndex()
	if err != nil {
		return nil, err
	}
	s.report.Protocol = dataformat.ClusterProtocol
	s.report.LegacyFormat = profile
	s.report.RestoreRequired = s.manifest.RestoreRequired

	ok = true
	return s, nil
}

func inspectSnapshots(ctx context.Context, dir string, key crypto.Key) (int, uint64, error) {
	count := 0
	var covered uint64
	// Open an existing snapshot directory only; inspection must not create one.
	snapdir := filepath.Join(dir, SnapshotDir)
	if info, err := os.Lstat(snapdir); err == nil {
		if !info.IsDir() {
			return 0, 0, errors.New("snapshots must be a directory")
		}
		if info, err := os.Lstat(filepath.Join(snapdir, "snapshots")); err != nil || !info.IsDir() {
			return 0, 0, errors.New("snapshot repository missing or not a directory")
		}
		staged, err := stageSnapshotRepository(ctx, snapdir)
		if err != nil {
			return 0, 0, err
		}
		defer func() { _ = os.RemoveAll(staged) }()
		store, err := raft.NewFileSnapshotStore(filepath.Join(staged, SnapshotDir), retainedSnapshots, io.Discard)
		if err != nil {
			return 0, 0, err
		}
		snapshots, err := store.List()
		if err != nil {
			return 0, 0, err
		}
		count = len(snapshots)
		for _, snap := range snapshots {
			if err := ctx.Err(); err != nil {
				return 0, 0, err
			}
			_, reader, err := store.Open(snap.ID)
			if err != nil {
				return 0, 0, err
			}
			// Validate a private copy; never mutate a historical snapshot in place.
			err = validatePhysicalSnapshot(reader, key)
			_ = reader.Close()
			if err != nil {
				return 0, 0, fmt.Errorf("snapshot %s: %w", snap.ID, err)
			}
			covered = max(covered, snap.Index)
		}
	} else if !os.IsNotExist(err) {
		return 0, 0, err
	}
	return count, covered, nil
}

func validatePhysicalSnapshot(r io.Reader, key crypto.Key) error {
	file, err := os.CreateTemp("", "lobslaw-snapshot-inspect-*.db")
	if err != nil {
		return err
	}
	path := file.Name()
	defer func() { _ = os.Remove(path) }()
	_, err = io.Copy(file, r)
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	db, err := prepareSnapshotDB(path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	cipher, err := crypto.NewCipher(key)
	if err != nil {
		return err
	}
	return upgradeStateDB(db, cipher)
}

func InspectData(ctx context.Context, dir, profile string, key crypto.Key) (DataInspection, error) {
	s, err := openDataSource(ctx, dir, profile, key)
	if err != nil {
		return DataInspection{}, err
	}
	defer s.close()
	return s.report, nil
}

// MigrateData builds a private, validated recovery directory and publishes it
// once. The original is the rollback copy. No active path is replaced, no log is
// rewritten, and interrupted candidates are never selected by startup.
func MigrateData(ctx context.Context, source, destination, profile string, key crypto.Key) (DataInspection, error) {
	var empty DataInspection
	src, err := filepath.Abs(source)
	if err != nil {
		return empty, err
	}
	dst, err := filepath.Abs(destination)
	if err != nil {
		return empty, err
	}
	if dst == src || strings.HasPrefix(dst, src+string(os.PathSeparator)) {
		return empty, errors.New("destination must be outside source")
	}
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		return empty, errors.New("destination must not exist")
	}
	s, err := openDataSource(ctx, src, profile, key)
	if err != nil {
		return empty, err
	}
	defer s.close()
	stage, err := os.MkdirTemp(filepath.Dir(dst), ".lobslaw-migrate-*")
	if err != nil {
		return empty, err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	for _, name := range []string{"state.db", RaftLogFile, SnapshotDir} {
		if err := copyDataTree(ctx, filepath.Join(src, name), filepath.Join(stage, name)); err != nil {
			return empty, err
		}
	}
	state, err := OpenStore(filepath.Join(stage, "state.db"), key)
	if err != nil {
		return empty, err
	}
	var migratedFormat StateFormat
	formatErr := state.loadDB().View(func(tx *bolt.Tx) error { var err error; migratedFormat, err = readStateFormat(tx); return err })
	if formatErr != nil {
		_ = state.Close()
		return empty, formatErr
	}
	if err := state.Close(); err != nil {
		return empty, err
	}
	manifest := dataformat.Manifest{StateVersion: migratedFormat.Version, LogVersion: dataformat.LogVersion, Version: dataformat.PhysicalVersion, Protocol: migratedFormat.Protocol, LegacyFormat: s.report.LegacyFormat, RestoreRequired: true}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return empty, err
	}
	if err := writeDataFile(filepath.Join(stage, dataformat.ManifestName), strings.NewReader(string(raw))); err != nil {
		return empty, err
	}
	if _, err := InspectData(ctx, stage, manifest.LegacyFormat, key); err != nil {
		return empty, err
	}
	if err := syncSnapshotDirectory(stage); err != nil {
		return empty, err
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if err := os.Rename(stage, dst); err != nil {
		return empty, err
	}
	if err := syncSnapshotDirectory(filepath.Dir(dst)); err != nil {
		return empty, fmt.Errorf("destination published at %s but directory sync failed: %w", dst, err)
	}
	s.report.State = migratedFormat
	s.report.RestoreRequired = true
	return s.report, nil
}

func copyDataTree(ctx context.Context, source, destination string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if os.IsNotExist(err) && filepath.Base(source) == SnapshotDir {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.Mkdir(destination, 0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyDataTree(ctx, filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
				return err
			}
		}
		return syncSnapshotDirectory(destination)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing non-regular data path %s", source)
	}
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return writeDataFile(destination, file)
}

func writeDataFile(path string, reader io.Reader) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(file, reader)
	err = errors.Join(err, file.Sync(), file.Close())
	return err
}

// AcceptDataRecovery is an explicit offline operator acknowledgement, never
// called by startup. Existing task state remains intact for deliberate review.
func AcceptDataRecovery(ctx context.Context, dir string, key crypto.Key) error {
	source, err := openDataSource(ctx, dir, "", key)
	if err != nil {
		return err
	}
	defer source.close()
	if !source.manifest.RestoreRequired {
		return errors.New("no physical recovery acknowledgement is pending")
	}
	source.manifest.RestoreRequired = false
	raw, err := json.MarshalIndent(source.manifest, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".data-accept-*")
	if err != nil {
		return err
	}
	path := file.Name()
	defer func() { _ = os.Remove(path) }()
	_, err = file.Write(raw)
	err = errors.Join(err, file.Sync(), file.Close())
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(path, filepath.Join(dir, dataformat.ManifestName)); err != nil {
		return err
	}
	return syncSnapshotDirectory(dir)
}
