package main

import (
	"path/filepath"
	"testing"
)

func TestBackupCLIRequiresRetentionAndEncryptionChoices(t *testing.T) {
	for _, args := range [][]string{
		{"prune", "--repository", t.TempDir()},
		{"create", "--repository", t.TempDir()},
		{"restore", "../unsafe", "--repository", t.TempDir()},
		{"prune", "--repository", filepath.Join(t.TempDir(), "backups"), "--keep-within", "-2d"},
	} {
		if err := runBackup(args); err == nil {
			t.Fatalf("accepted invalid backup arguments: %v", args)
		}
	}
}
