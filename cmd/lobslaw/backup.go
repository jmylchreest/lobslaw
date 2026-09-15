package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/jmylchreest/lobslaw/internal/archive"
	"github.com/jmylchreest/lobslaw/internal/backup"
	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/config"
)

func dispatchBackup(args []string) bool {
	i := findSubcmd(args, "backup")
	if i < 0 {
		return false
	}
	if err := runBackup(args[i+1:]); err != nil {
		fmt.Fprintf(os.Stderr, "backup: %v\n", err)
		os.Exit(1)
	}
	return true
}

func runBackup(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lobslaw backup create|list|restore|prune|pin|unpin --repository PATH")
	}
	switch args[0] {
	case "create":
		return backupCreate(args[1:])
	case "restore":
		return backupRestore(args[1:])
	case "list", "prune", "pin", "unpin":
		return backupManage(args[0], args[1:])
	default:
		return fmt.Errorf("unknown backup command %q", args[0])
	}
}

func backupCreate(args []string) error {
	args, offline := takeOffline(args)
	fs := flag.NewFlagSet("backup create", flag.ContinueOnError)
	var local offlineStore
	var node liveNode
	if offline {
		local.bind(fs)
	} else {
		node.bind(fs)
	}
	path := fs.String("repository", "", "local backup repository directory")
	sourceID := fs.String("source-id", envOr("LOBSLAW_ARCHIVE_SOURCE_ID", ""), "stable source identity shared by every generation")
	var recipients []age.Recipient
	fs.Func("recipient", "age recipient (repeatable; required)", func(value string) error {
		recipient, err := age.ParseX25519Recipient(value)
		if err != nil {
			return err
		}
		recipients = append(recipients, recipient)
		return nil
	})
	pos, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *path == "" || len(recipients) == 0 {
		return errors.New("backup create requires --repository and --recipient, with no positional arguments")
	}
	var snapshot archive.Snapshot
	if offline {
		if err := config.LoadDotenv(envOr("LOBSLAW_ENV", "")); err != nil {
			return err
		}
		db, err := local.resolveStatePath()
		if err != nil {
			return err
		}
		key, err := local.resolveKey()
		if err != nil {
			return err
		}
		snapshot.Records, err = memory.ReadArchiveRecords(context.Background(), db, key)
		if err != nil {
			return err
		}
		snapshot.Manifest = archive.Manifest{
			SnapshotID: ids.New(), CreatedAt: time.Now().UTC(), SourceVersion: Version,
			Omissions: []string{"embeddings", "credentials and certificates", "policy grants", "Raft metadata", "filesystem attachments", "execution audit history"},
		}
	} else {
		snapshot, err = exportArchiveSnapshot(&node)
		if err != nil {
			return err
		}
	}
	snapshot.Manifest.SourceID, err = archive.SourceIdentity(snapshot.Manifest, *sourceID)
	if err != nil {
		return err
	}
	generation, err := (backup.Repository{Path: *path}).Create(snapshot, recipients...)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(generation)
}

func backupRestore(args []string) error {
	fs := flag.NewFlagSet("backup restore", flag.ContinueOnError)
	var node liveNode
	node.bind(fs)
	node.timeout = 30 * time.Minute
	var opts memory.ArchiveImportOptions
	bindArchiveImportOptions(fs, &opts)
	path := fs.String("repository", "", "local backup repository directory")
	identityPath := fs.String("identity", "", "age identity file")
	apply := fs.Bool("apply", false, "restore into an empty knowledge store through Raft")
	pos, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *path == "" || *identityPath == "" {
		return errors.New("restore requires snapshot id, --repository and --identity")
	}
	file, err := os.Open(*identityPath)
	if err != nil {
		return err
	}
	identities, err := age.ParseIdentities(file)
	_ = file.Close()
	if err != nil {
		return err
	}
	snapshot, err := (backup.Repository{Path: *path}).Read(pos[0], identities...)
	if err != nil {
		return err
	}
	return importArchiveSnapshot(&node, snapshot, opts, *apply, true)
}

func backupManage(command string, args []string) error {
	fs := flag.NewFlagSet("backup "+command, flag.ContinueOnError)
	path := fs.String("repository", "", "local backup repository directory")
	keepLast := fs.Int("keep-last", 0, "retain at least this many newest generations")
	keepWithin := fs.String("keep-within", "", "retain generations within this age, e.g. 30d")
	apply := fs.Bool("apply", false, "delete verified unpinned generations in the prune plan")
	pos, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if *path == "" {
		return errors.New("--repository is required")
	}
	repo := backup.Repository{Path: *path}
	if command == "pin" || command == "unpin" {
		if len(pos) != 1 {
			return errors.New("one snapshot id required")
		}
		return repo.Pin(pos[0], command == "pin")
	}
	if len(pos) != 0 {
		return errors.New("unexpected backup argument")
	}
	if command == "list" {
		generations, err := repo.List()
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(generations)
	}
	age, err := backupRetentionAge(*keepWithin)
	if err != nil {
		return err
	}
	plan, err := repo.PlanPrune(backup.Retention{KeepLast: *keepLast, KeepWithin: age}, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(plan); err != nil {
		return err
	}
	if *apply {
		return repo.Prune(plan)
	}
	return nil
}

func backupRetentionAge(value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(value, "d"); ok {
		n, err := strconv.ParseInt(days, 10, 32)
		if err != nil || n <= 0 || n > 100000 {
			return 0, errors.New("retention days must be between 1 and 100000")
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, errors.New("retention age must be a positive duration")
	}
	return duration, nil
}
