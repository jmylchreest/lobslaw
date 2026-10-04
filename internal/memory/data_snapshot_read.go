package memory

import (
	"context"
	"os"
	"path/filepath"
)

// HashiCorp's snapshot store constructor creates and removes a permission-test
// file. Invoke it only on a private copy: inspection must also work on read-only
// backups and must never follow a source permission-test symlink. The existing
// copier rejects symlinks and special files before the snapshot library sees them.
func stageSnapshotRepository(ctx context.Context, source string) (string, error) {
	stage, err := os.MkdirTemp("", "lobslaw-snapshot-repository-*")
	if err != nil {
		return "", err
	}
	if err := copyDataTree(ctx, source, filepath.Join(stage, SnapshotDir)); err != nil {
		_ = os.RemoveAll(stage)
		return "", err
	}
	return stage, nil
}
