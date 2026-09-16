package memory

import (
	"errors"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/archive"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// An ordinary replacement follows the saved mapping. Replacing the original is
// a separate, explicit operation: retire the alongside group and point the same
// source mapping back at the original ID within the backed-up transaction.
func planArchiveOriginalReplacement(existing, incoming []archive.Record, opts ArchiveImportOptions) (ArchiveImportPlan, error) {
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
	selections := opts.ReplaceOriginal
	opts.ReplaceOriginal = nil
	opts.Replace = append(append([]ArchiveRecordRef{}, opts.Replace...), selections...)
	if _, err := replacementSelections(source, opts); err != nil {
		return empty, err
	}
	targets, mappings, err := archiveImportTargets(source, destination, opts)
	if err != nil {
		return empty, err
	}
	retired := make(map[archiveRecordKey]bool)
	var moved []ArchiveRecordRef
	for _, ref := range selections {
		key := archiveRecordKey{ref.Kind, ref.ID}
		switch ref.Kind {
		case "sessions", "scheduled-tasks":
		default:
			return empty, errors.New("replace-original supports sessions and scheduled-tasks only")
		}
		mapping := mappings[key]
		if mapping == nil {
			return empty, errors.New("replace-original requires an existing import mapping; use replace for a first import")
		}
		original, err := mapArchiveOwner(ref.ID, proto.Clone(source[key]), opts.Owners)
		if err != nil {
			return empty, err
		}
		if mapping.DestinationId == original {
			continue
		}
		retired[archiveRecordKey{ref.Kind, mapping.DestinationId}] = true
		retired[archiveRecordKey{ref.Kind, original}] = true
		targets[key] = original
		moved = append(moved, ref)
	}
	if len(moved) == 0 {
		return PlanArchiveImport(existing, incoming, opts)
	}
	// Transcript identity follows the remapped parent, including messages that
	// did not exist when the alongside import was first made.
	for key, msg := range source {
		if message, ok := msg.(*lobslawv1.SessionMessage); ok {
			if parent, ok := targets[archiveRecordKey{"sessions", message.SessionId}]; ok {
				targets[key] = sessionMessageKey(parent, message.Seq)
			}
		}
	}
	retained, removed := retireArchiveGroups(existing, destination, retired, opts.SourceID)
	opts.targets = targets
	plan, err := PlanArchiveImport(retained, incoming, opts)
	if err != nil {
		return plan, err
	}
	plan.removed = append(plan.removed, removed...)
	plan.Replaced = append(plan.Replaced, moved...)
	return plan, nil
}

func retireArchiveGroups(existing []archive.Record, destination map[archiveRecordKey]proto.Message, retired map[archiveRecordKey]bool, sourceID string) ([]archive.Record, []ArchiveRecordRef) {
	var retained []archive.Record
	var removed []ArchiveRecordRef
	for _, record := range existing {
		key := archiveRecordKey{record.Kind, record.ID}
		remove := retired[archiveRecordGroup(key)]
		if mapping, ok := destination[key].(*lobslawv1.ArchiveMapping); ok {
			remove = mapping.SourceId == sourceID && retired[archiveRecordGroup(archiveRecordKey{mapping.Kind, mapping.DestinationId})]
		}
		if remove {
			removed = append(removed, ArchiveRecordRef{Kind: record.Kind, ID: record.ID})
		} else {
			retained = append(retained, record)
		}
	}
	return retained, removed
}
