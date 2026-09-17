package memory

import (
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/archive"
	"github.com/jmylchreest/lobslaw/internal/ids"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// ArchiveStateDigest binds a replacement to the portable contents of a verified
// backup. Embeddings and runtime claims are deliberately outside that snapshot.
func ArchiveStateDigest(records []archive.Record) (string, error) {
	return archiveImportID(records, ArchiveImportOptions{})
}

func planArchiveReplacement(existing, incoming []archive.Record, opts ArchiveImportOptions) (ArchiveImportPlan, error) {
	var empty ArchiveImportPlan
	if opts.SourceID == "" {
		return empty, errors.New("replacement requires a stable --source-id")
	}
	source, err := indexArchiveRecords(incoming)
	if err != nil {
		return empty, err
	}
	destination, err := indexArchiveRecords(existing)
	if err != nil {
		return empty, err
	}
	targets, _, err := archiveImportTargets(source, destination, opts)
	if err != nil {
		return empty, err
	}
	selected, err := replacementSelections(source, opts)
	if err != nil {
		return empty, err
	}
	selections := opts.Replace
	opts.Replace = nil
	base, err := PlanArchiveImport(existing, incoming, opts)
	if err != nil {
		return base, err
	}
	active := make(map[archiveRecordKey]bool)
	for key := range selected {
		target := archiveRecordKey{key.kind, targets[key]}
		for _, conflict := range base.Conflicts {
			ck := archiveRecordKey{conflict.Kind, conflict.ID}
			if archiveRecordGroup(ck) == key || archiveRecordGroup(ck) == target {
				active[target] = true
			}
		}
	}
	if len(active) == 0 {
		return base, nil
	}
	var retained []archive.Record
	var removed []ArchiveRecordRef
	for _, record := range existing {
		key := archiveRecordKey{record.Kind, record.ID}
		remove := active[archiveRecordGroup(key)]
		if mapping, ok := destination[key].(*lobslawv1.ArchiveMapping); ok {
			remove = mapping.SourceId == opts.SourceID && active[archiveRecordGroup(archiveRecordKey{mapping.Kind, mapping.DestinationId})]
		}
		if remove {
			removed = append(removed, ArchiveRecordRef{Kind: record.Kind, ID: record.ID})
		} else {
			retained = append(retained, record)
		}
	}
	opts.targets = targets
	plan, err := PlanArchiveImport(retained, incoming, opts)
	if err != nil {
		return plan, err
	}
	plan.Removed = removed
	for _, ref := range selections {
		if active[archiveRecordKey{ref.Kind, targets[archiveRecordKey{ref.Kind, ref.ID}]}] {
			plan.Replaced = append(plan.Replaced, ref)
		}
	}
	return plan, nil
}

func replacementSelections(source map[archiveRecordKey]proto.Message, opts ArchiveImportOptions) (map[archiveRecordKey]bool, error) {
	selected := make(map[archiveRecordKey]bool)
	for _, ref := range opts.Replace {
		key := archiveRecordKey{ref.Kind, ref.ID}
		if source[key] == nil || selected[key] {
			return nil, fmt.Errorf("invalid or duplicate replacement selection %s/%s", ref.Kind, ref.ID)
		}
		switch ref.Kind {
		case "session-messages", "import-mappings", "skill-blobs":
			return nil, errors.New("replace the complete session or record, not individual messages, provenance or immutable blobs")
		}
		selected[key] = true
	}
	for _, ref := range append(append([]ArchiveRecordRef{}, opts.Skip...), opts.Alongside...) {
		if selected[archiveRecordKey{ref.Kind, ref.ID}] {
			return nil, errors.New("record cannot have multiple conflict resolutions")
		}
	}
	return selected, nil
}

func archiveRecordGroup(key archiveRecordKey) archiveRecordKey {
	if key.kind == "session-messages" {
		if split := strings.LastIndex(key.id, ":"); split > 0 {
			return archiveRecordKey{"sessions", key.id[:split]}
		}
	}
	return key
}

// Replacements use one bounded transaction. Its state guard covers deletions,
// edits and new transcript messages arriving after the backup was taken.
func prepareArchiveReplacement(batch *lobslawv1.ArchiveBatch, plan ArchiveImportPlan, digest string) error {
	if len(plan.Replaced) == 0 {
		return nil
	}
	batch.BackupDigest = digest
	removed := make(map[archiveRecordKey]bool)
	for _, ref := range plan.Removed {
		removed[archiveRecordKey{ref.Kind, ref.ID}] = true
	}
	for _, mutation := range batch.Records {
		key := archiveRecordKey{mutation.Kind, mutation.Id}
		if !mutation.DependencyOnly && removed[key] {
			mutation.Replace = true
			delete(removed, key)
		}
	}
	for _, ref := range plan.Removed {
		if removed[archiveRecordKey{ref.Kind, ref.ID}] {
			batch.Records = append(batch.Records, &lobslawv1.ArchiveMutation{Kind: ref.Kind, Id: ref.ID, Delete: true})
		}
	}
	// Provenance makes retries idempotent. A new transaction must not reuse an
	// older receipt if a later recovery restored the same portable state.
	batch.BatchId = ids.New()
	if proto.Size(batch) > maxArchiveBatchBytes {
		return errors.New("atomic replacement exceeds archive batch size limit; import a smaller selection")
	}
	return nil
}
