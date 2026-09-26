package main

import (
	"bytes"
	"errors"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/logging"
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

func TestImportHelpDefaultsAndRestoreFlags(t *testing.T) {
	for _, command := range []string{"backup restore", "archive import"} {
		t.Run(command, func(t *testing.T) {
			output, err := os.CreateTemp(t.TempDir(), "help")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = output.Close() }()
			previous := os.Stderr
			os.Stderr = output
			defer func() { os.Stderr = previous }()
			oldLogger := slog.Default()
			defer slog.SetDefault(oldLogger)
			var logs bytes.Buffer
			slog.SetDefault(logging.New(&logs, slog.LevelInfo, logging.FormatJSON))
			if command == "backup restore" {
				if !dispatchBackup([]string{"backup", "restore", "--help"}) {
					t.Fatal("backup not dispatched")
				}
			} else if err := archiveImport([]string{"--help"}, false); !errors.Is(err, flag.ErrHelp) {
				t.Fatal(err)
			}
			if logs.Len() != 0 {
				t.Fatalf("help became diagnostics: %s", logs.String())
			}
			data, err := os.ReadFile(output.Name())
			if err != nil {
				t.Fatal(err)
			}
			help := string(data)
			if !strings.Contains(help, command) || !strings.Contains(help, "(default "+defaultArchiveImportTimeout.String()+")") {
				t.Fatalf("missing usage or actual timeout default: %s", help)
			}
			for _, name := range []string{"skip", "alongside", "replace", "replace-original", "keep-existing"} {
				advertised := strings.Contains(help, "  -"+name+" ") || strings.Contains(help, "  -"+name+"\n")
				if advertised != (command == "archive import") {
					t.Errorf("unexpected flag %q advertisement in %s", name, help)
				}
			}
		})
	}
}

func TestLiveNodeTimeoutDefaultsAndOverrides(t *testing.T) {
	for _, timeout := range []time.Duration{defaultLiveNodeTimeout, defaultArchiveImportTimeout} {
		fs := flag.NewFlagSet("timeout", flag.ContinueOnError)
		var node liveNode
		if timeout == defaultLiveNodeTimeout {
			node.bind(fs)
		} else {
			node.bindWithTimeout(fs, timeout)
		}
		if err := fs.Parse(nil); err != nil {
			t.Fatal(err)
		}
		if node.timeout != timeout || fs.Lookup("timeout").DefValue != timeout.String() {
			t.Fatalf("runtime %s and help %s must share default %s", node.timeout, fs.Lookup("timeout").DefValue, timeout)
		}
		const override = 7 * time.Minute
		if err := fs.Parse([]string{"--timeout", override.String()}); err != nil {
			t.Fatal(err)
		}
		if node.timeout != override {
			t.Fatalf("explicit timeout ignored: %s", node.timeout)
		}
	}
}

func TestBackupRestoreRejectsConflictFlags(t *testing.T) {
	for _, name := range []string{"skip", "alongside", "replace", "replace-original", "keep-existing"} {
		if err := backupRestore([]string{"--" + name + "=sessions/example"}); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatalf("restore accepted unsupported --%s: %v", name, err)
		}
	}
}
