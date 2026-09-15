package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"filippo.io/age"

	"github.com/jmylchreest/lobslaw/internal/archive"
	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/config"
)

const archiveUsage = `lobslaw archive — portable versioned knowledge archives

  export --offline --state-db PATH --out FILE --recipient age1...
  export --context NAME --out FILE --recipient age1...
  import FILE --context NAME --identity PATH [--apply]
  inspect FILE --identity PATH
  verify FILE --identity PATH

Offline export requires a stopped source node or a consistent database snapshot and its
memory key (--memory-key-ref, --config or LOBSLAW_MEMORY_KEY). It includes stored
memories, skills and their versions, soul/pinned content, sessions, preferences,
schedules and commitments. Embeddings, credentials and Raft state are excluded.

Encryption is the default; --plaintext explicitly opts out. --recipient may be
repeated. Existing output files are never replaced. inspect prints only the
verified manifest, not memory content. Import defaults to a preview; --apply writes
through Raft. Repeat the same archive and owner mappings with --apply to resume.
Use --owner source=destination for each nonempty identity and --source-timezone
for cron expressions without an explicit timezone. Restored jobs remain paused.
`

func dispatchArchive(args []string) bool {
	i := findSubcmd(args, "archive")
	if i < 0 {
		return false
	}
	args = args[i+1:]
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, archiveUsage)
		os.Exit(2)
	}

	var err error
	switch args[0] {
	case "export":
		if _, offline := takeOffline(args[1:]); offline {
			err = archiveExport(args[1:])
		} else {
			err = archiveExportLive(args[1:])
		}
	case "import":
		err = archiveImport(args[1:], false)
	case "inspect", "verify":
		err = archiveInspect(args[1:], args[0] == "inspect")
	default:
		err = fmt.Errorf("unknown archive command %q", args[0])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "archive: %v\n", err)
		os.Exit(1)
	}
	return true
}

func archiveExport(args []string) error {
	fs := flag.NewFlagSet("archive export", flag.ContinueOnError)
	var opts offlineStore
	opts.bind(fs)
	sourceID := fs.String("source-id", envOr("LOBSLAW_ARCHIVE_SOURCE_ID", ""), "stable source identity shared by every generation")
	offline := fs.Bool("offline", false, "read a stopped node or consistent snapshot")
	out := fs.String("out", "", "new output archive path")
	plaintext := fs.Bool("plaintext", false, "explicitly write an unencrypted archive")
	var recipients []age.Recipient
	fs.Func("recipient", "age recipient (repeatable)", func(value string) error {
		r, err := age.ParseX25519Recipient(value)
		if err != nil {
			return err
		}
		recipients = append(recipients, r)
		return nil
	})
	positional, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return errors.New("unexpected export argument")
	}
	if !*offline {
		return errors.New("live export is not implemented; use --offline with a stopped source or snapshot")
	}
	if *out == "" {
		return errors.New("--out is required")
	}
	if *plaintext && len(recipients) > 0 {
		return errors.New("--plaintext cannot be combined with --recipient")
	}
	if !*plaintext && len(recipients) == 0 {
		return errors.New("--recipient is required unless --plaintext is set")
	}

	if err := config.LoadDotenv(envOr("LOBSLAW_ENV", "")); err != nil {
		return err
	}
	path, err := opts.resolveStatePath()
	if err != nil {
		return err
	}
	key, err := opts.resolveKey()
	if err != nil {
		return err
	}
	records, err := memory.ReadArchiveRecords(context.Background(), path, key)
	if err != nil {
		return err
	}
	snapshot := archive.Snapshot{
		Records: records,
		Manifest: archive.Manifest{
			SourceID:      *sourceID,
			SnapshotID:    ids.New(),
			CreatedAt:     time.Now().UTC(),
			SourceVersion: Version,
			Omissions: []string{
				"embeddings",
				"credentials and certificates",
				"Raft and runtime state",
				"policy grants",
				"audit trail",
				"filesystem attachments",
				"self-taught usage counters",
			},
		},
	}

	if err := publishArchive(*out, func(w io.Writer) error {
		return archive.Write(w, snapshot, recipients...)
	}); err != nil {
		return err
	}
	fmt.Printf("Exported %d records to %s (snapshot %s)\n", len(records), *out, snapshot.Manifest.SnapshotID)
	return nil
}

// Link publishes a completed private temporary file without replacing an
// existing generation, including one created concurrently by another exporter.
func publishArchive(path string, write func(io.Writer) error) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".lobslaw-archive-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	defer func() { _ = f.Close() }()
	if err := write(f); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Link(f.Name(), path); err != nil {
		return fmt.Errorf("publish archive (existing files are not overwritten): %w", err)
	}
	return nil
}

func archiveInspect(args []string, inspect bool) error {
	fs := flag.NewFlagSet("archive verify", flag.ContinueOnError)
	identity := fs.String("identity", "", "age identity file")
	pos, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("one archive path required")
	}

	var identities []age.Identity
	if *identity != "" {
		f, err := os.Open(*identity)
		if err != nil {
			return err
		}
		identities, err = age.ParseIdentities(f)
		_ = f.Close()
		if err != nil {
			return err
		}
	}

	f, err := os.Open(pos[0])
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	snapshot, err := archive.Read(f, identities...)
	if err != nil {
		return err
	}

	if inspect {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(snapshot.Manifest)
	}
	fmt.Printf("Verified snapshot %s: %d records\n", snapshot.Manifest.SnapshotID, len(snapshot.Records))
	return nil
}
