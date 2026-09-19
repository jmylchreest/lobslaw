package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"

	"github.com/jmylchreest/lobslaw/internal/archive"
	"github.com/jmylchreest/lobslaw/internal/backup"
	"github.com/jmylchreest/lobslaw/internal/memory"
)

func resolveArchiveImport(node *liveNode, snapshot archive.Snapshot, opts memory.ArchiveImportOptions, apply, requireEmpty, interactive bool, backupPath, identityPath string) error {
	reader := bufio.NewReader(os.Stdin)
	for {
		response, err := requestArchiveImport(node, snapshot, opts, false, requireEmpty)
		if err != nil {
			return err
		}
		var plan memory.ArchiveImportPlan
		if err := json.Unmarshal(response.PlanJson, &plan); err != nil {
			return err
		}
		if len(plan.Conflicts) > 0 && interactive {
			if err := promptArchiveConflicts(reader, os.Stderr, plan, &opts); err != nil {
				return err
			}
			continue
		}
		if !apply {
			fmt.Println(string(response.PlanJson))
			return nil
		}
		if len(plan.Conflicts) > 0 && (!opts.KeepExisting || len(plan.ConflictDetails) > 0) {
			return errors.New("unresolved conflicts; use --interactive or explicit conflict selections")
		}
		if len(plan.Replaced) > 0 {
			opts.BackupDigest, err = backupArchiveDestination(node, backupPath, identityPath)
			if err != nil {
				return err
			}
		}
		return importArchiveSnapshot(node, snapshot, opts, true, requireEmpty)
	}
}

func promptArchiveConflicts(reader *bufio.Reader, out io.Writer, plan memory.ArchiveImportPlan, opts *memory.ArchiveImportOptions) error {
	seen := make(map[memory.ArchiveRecordRef]bool)
	for _, ref := range plan.Conflicts {
		if ref.Kind == "session-messages" {
			split := strings.LastIndex(ref.ID, ":")
			if split <= 0 {
				return errors.New("invalid session message conflict")
			}
			ref = memory.ArchiveRecordRef{Kind: "sessions", ID: ref.ID[:split]}
		}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		mapped := false
		for _, detail := range plan.ConflictDetails {
			detailRef := memory.ArchiveRecordRef{Kind: detail.Kind, ID: detail.ID}
			if detailRef.Kind == "session-messages" {
				if split := strings.LastIndex(detailRef.ID, ":"); split > 0 {
					detailRef = memory.ArchiveRecordRef{Kind: "sessions", ID: detailRef.ID[:split]}
				}
			}
			if detailRef == ref {
				_, _ = fmt.Fprintf(out, "%s/%s: %s (destination %s)\n", ref.Kind, ref.ID, detail.Reason, detail.DestinationID)
				mapped = true
			}
		}
		canReplaceOriginal := mapped && (ref.Kind == "sessions" || ref.Kind == "scheduled-tasks")
		for _, selected := range opts.ReplaceOriginal {
			if selected == ref {
				// Repeating the same blocked retirement cannot resolve an edit.
				canReplaceOriginal = false
			}
		}
		for {
			choices := "replace/alongside/skip/cancel"
			if mapped {
				choices = "replace/skip/cancel (replace updates the mapped copy)"
				if canReplaceOriginal {
					choices = "replace/replace-original/skip/cancel (replace-original retires the mapped copy if unchanged)"
				}
			}
			_, _ = fmt.Fprintf(out, "%s/%s [%s]: ", ref.Kind, ref.ID, choices)
			answer, err := reader.ReadString('\n')
			if err != nil {
				return errors.New("conflict resolution cancelled; no records written")
			}
			answer = strings.ToLower(strings.TrimSpace(answer))
			if answer == "cancel" || answer == "" {
				return errors.New("conflict resolution cancelled; no records written")
			}
			if answer != "replace" && answer != "skip" && (answer != "alongside" || mapped) && (answer != "replace-original" || !canReplaceOriginal) {
				continue
			}
			// A fresh choice supersedes the earlier choice if re-planning found a
			// concurrent change. Never accumulate contradictory selections.
			opts.ReplaceOriginal = withoutArchiveRef(opts.ReplaceOriginal, ref)
			opts.Replace = withoutArchiveRef(opts.Replace, ref)
			opts.Skip = withoutArchiveRef(opts.Skip, ref)
			opts.Alongside = withoutArchiveRef(opts.Alongside, ref)
			switch answer {
			case "replace-original":
				opts.ReplaceOriginal = append(opts.ReplaceOriginal, ref)
			case "replace":
				opts.Replace = append(opts.Replace, ref)
			case "skip":
				opts.Skip = append(opts.Skip, ref)
			case "alongside":
				opts.Alongside = append(opts.Alongside, ref)
			}
			break
		}
	}
	return nil
}

func withoutArchiveRef(refs []memory.ArchiveRecordRef, target memory.ArchiveRecordRef) []memory.ArchiveRecordRef {
	var out []memory.ArchiveRecordRef
	for _, ref := range refs {
		if ref != target {
			out = append(out, ref)
		}
	}
	return out
}

func backupArchiveDestination(node *liveNode, path, identityPath string) (string, error) {
	if path == "" || identityPath == "" {
		return "", errors.New("replacement requires --backup-repository and --backup-identity")
	}
	file, err := os.Open(identityPath)
	if err != nil {
		return "", err
	}
	identities, err := age.ParseIdentities(file)
	_ = file.Close()
	if err != nil {
		return "", err
	}
	var recipients []age.Recipient
	for _, identity := range identities {
		if key, ok := identity.(*age.X25519Identity); ok {
			recipients = append(recipients, key.Recipient())
		}
	}
	if len(recipients) == 0 {
		return "", errors.New("backup identity must contain an X25519 age identity")
	}
	snapshot, err := exportArchiveSnapshot(node)
	if err != nil {
		return "", err
	}
	repo := backup.Repository{Path: path}
	generation, err := repo.Create(snapshot, recipients...)
	if err != nil {
		return "", err
	}
	verified, err := repo.Read(generation.Manifest.SnapshotID, identities...)
	if err != nil {
		return "", err
	}
	if err := repo.Pin(generation.Manifest.SnapshotID, true); err != nil {
		return "", err
	}
	digest, err := memory.ArchiveStateDigest(verified.Records)
	if err != nil {
		return "", err
	}
	expected, err := memory.ArchiveStateDigest(snapshot.Records)
	if err != nil {
		return "", err
	}
	if digest != expected {
		return "", errors.New("verified backup differs from destination export")
	}
	noticef("Verified and pinned destination backup: %s\n", generation.Manifest.SnapshotID)
	return digest, nil
}
