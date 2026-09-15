package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/jmylchreest/lobslaw/internal/archive"
	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func readArchiveFile(path, identityPath string) (archive.Snapshot, error) {
	var identities []age.Identity
	if identityPath != "" {
		file, err := os.Open(identityPath)
		if err != nil {
			return archive.Snapshot{}, err
		}
		identities, err = age.ParseIdentities(file)
		_ = file.Close()
		if err != nil {
			return archive.Snapshot{}, err
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return archive.Snapshot{}, err
	}
	defer func() { _ = file.Close() }()
	return archive.Read(file, identities...)
}

func bindArchiveImportOptions(fs *flag.FlagSet, opts *memory.ArchiveImportOptions) {
	fs.StringVar(&opts.SourceID, "source-id", "", "stable source identity (required for alongside on older archives)")
	fs.Func("skip", "keep the destination and skip source kind/id (repeatable; sessions include their messages)", func(value string) error {
		kind, id, ok := strings.Cut(value, "/")
		if !ok || kind == "" || id == "" {
			return errors.New("skip requires kind/id")
		}
		opts.Skip = append(opts.Skip, memory.ArchiveRecordRef{Kind: kind, ID: id})
		return nil
	})
	fs.Func("alongside", "import kind/id as a separate, persistently mapped copy (repeatable)", func(value string) error {
		kind, id, ok := strings.Cut(value, "/")
		if !ok || kind == "" || id == "" {
			return errors.New("alongside requires kind/id")
		}
		opts.Alongside = append(opts.Alongside, memory.ArchiveRecordRef{Kind: kind, ID: id})
		return nil
	})
	opts.Owners = make(map[string]string)
	fs.StringVar(&opts.SourceTimezone, "source-timezone", "", "source cron timezone, e.g. Europe/London")
	fs.BoolVar(&opts.KeepExisting, "keep-existing", false, "explicitly skip conflicting destination records")
	fs.Func("owner", "explicit source=destination identity mapping (repeatable, including unchanged identities)", func(value string) error {
		from, to, ok := strings.Cut(value, "=")
		if !ok || strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
			return errors.New("owner mapping must be source=destination with nonempty identities")
		}
		if _, exists := opts.Owners[from]; exists {
			return fmt.Errorf("duplicate owner mapping for %q", from)
		}
		opts.Owners[from] = to
		return nil
	})
}

func archiveImport(args []string, requireEmpty bool) error {
	fs := flag.NewFlagSet("archive import", flag.ContinueOnError)
	var node liveNode
	node.bind(fs)
	node.timeout = 30 * time.Minute
	var opts memory.ArchiveImportOptions
	bindArchiveImportOptions(fs, &opts)
	identity := fs.String("identity", "", "age identity file")
	apply := fs.Bool("apply", false, "apply the validated import through Raft")
	pos, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("one archive path required")
	}
	snapshot, err := readArchiveFile(pos[0], *identity)
	if err != nil {
		return err
	}
	return importArchiveSnapshot(&node, snapshot, opts, *apply, requireEmpty)
}

func importArchiveSnapshot(node *liveNode, snapshot archive.Snapshot, opts memory.ArchiveImportOptions, apply, requireEmpty bool) error {
	sourceID, err := archive.SourceIdentity(snapshot.Manifest, opts.SourceID)
	if err != nil {
		return err
	}
	opts.SourceID = sourceID
	// Verify and serialize before opening a connection. The private backup
	// identity stays here; plaintext records travel only through mutual TLS.
	var payload bytes.Buffer
	if err := archive.Write(&payload, snapshot); err != nil {
		return err
	}
	options, err := json.Marshal(opts)
	if err != nil {
		return err
	}
	conn, err := node.dial()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := node.ctx()
	defer cancel()
	stream, err := lobslawv1.NewArchiveServiceClient(conn).ImportArchive(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&lobslawv1.ArchiveChunk{OptionsJson: options, Apply: apply, RequireEmpty: requireEmpty}); err != nil {
		return err
	}
	for payload.Len() > 0 {
		if err := stream.Send(&lobslawv1.ArchiveChunk{Data: payload.Next(memory.ArchiveChunkBytes)}); err != nil {
			return err
		}
	}
	response, err := stream.CloseAndRecv()
	if err != nil {
		return err
	}
	if len(response.PlanJson) > 0 {
		fmt.Println(string(response.PlanJson))
	}
	if len(response.ResultJson) > 0 {
		fmt.Println(string(response.ResultJson))
	}
	if response.Error != "" {
		return errors.New(response.Error)
	}
	return nil
}

func exportArchiveSnapshot(node *liveNode) (archive.Snapshot, error) {
	conn, err := node.dial()
	if err != nil {
		return archive.Snapshot{}, err
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := node.ctx()
	defer cancel()
	stream, err := lobslawv1.NewArchiveServiceClient(conn).ExportArchive(ctx, &lobslawv1.ArchiveExportRequest{})
	if err != nil {
		return archive.Snapshot{}, err
	}
	var payload bytes.Buffer
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return archive.Snapshot{}, err
		}
		if len(chunk.Data) > memory.ArchiveChunkBytes || int64(payload.Len()+len(chunk.Data)) > archive.MaxArchiveBytes {
			return archive.Snapshot{}, errors.New("archive stream exceeds size limit")
		}
		payload.Write(chunk.Data)
	}
	return archive.Read(&payload)
}

func archiveExportLive(args []string) error {
	fs := flag.NewFlagSet("archive export", flag.ContinueOnError)
	var node liveNode
	node.bind(fs)
	out := fs.String("out", "", "new output archive path")
	sourceID := fs.String("source-id", envOr("LOBSLAW_ARCHIVE_SOURCE_ID", ""), "stable source identity shared by every generation")
	plaintext := fs.Bool("plaintext", false, "explicitly write an unencrypted archive")
	var recipients []age.Recipient
	fs.Func("recipient", "age recipient (repeatable)", func(value string) error {
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
	if len(pos) != 0 || *out == "" {
		return errors.New("export requires --out and no positional arguments")
	}
	if *plaintext == (len(recipients) > 0) {
		return errors.New("choose --recipient or explicit --plaintext")
	}
	snapshot, err := exportArchiveSnapshot(&node)
	if err != nil {
		return err
	}
	snapshot.Manifest.SourceID, err = archive.SourceIdentity(snapshot.Manifest, *sourceID)
	if err != nil {
		return err
	}
	return publishArchive(*out, func(dst io.Writer) error { return archive.Write(dst, snapshot, recipients...) })
}
