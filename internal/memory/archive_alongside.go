package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/jmylchreest/lobslaw/internal/archive"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type ArchiveConflict struct {
	Kind          string `json:"kind"`
	ID            string `json:"id"`
	DestinationID string `json:"destination_id"`
	Reason        string `json:"reason"`
}

func archiveMappingID(source, kind, id string) string {
	raw, _ := json.Marshal([]string{source, kind, id})
	return strings.TrimPrefix(Digest(raw), "sha256:")
}

func archiveFingerprint(msg proto.Message) (string, error) {
	copy := proto.Clone(msg)
	if err := portableMessage(copy.ProtoReflect()); err != nil {
		return "", err
	}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(copy)
	if err != nil {
		return "", err
	}
	return Digest(raw), nil
}

// PlanArchiveImport resolves durable source identities before the ordinary
// conflict planner. A snapshot-specific receipt must not hide edits or deletions
// to a mapped record, even when the caller repeats the very same archive.
func PlanArchiveImport(existing, incoming []archive.Record, opts ArchiveImportOptions) (ArchiveImportPlan, error) {
	if len(opts.ReplaceOriginal) > 0 {
		return planArchiveOriginalReplacement(existing, incoming, opts)
	}
	if len(opts.Replace) > 0 {
		return planArchiveReplacement(existing, incoming, opts)
	}
	if len(opts.Skip) > 0 {
		filtered, skipped, err := skipArchiveRecords(incoming, opts)
		if err != nil {
			return ArchiveImportPlan{}, err
		}
		opts.Skip = nil
		plan, err := PlanArchiveImport(existing, filtered, opts)
		plan.Skipped = skipped
		return plan, err
	}
	if opts.SourceID == "" {
		if len(opts.Alongside) != 0 {
			return ArchiveImportPlan{}, errors.New("alongside import requires a stable --source-id")
		}
		return planArchiveRecords(existing, incoming, opts)
	}
	if strings.TrimSpace(opts.SourceID) != opts.SourceID || len(opts.SourceID) > 256 {
		return ArchiveImportPlan{}, errors.New("source id must be nonempty, trimmed and at most 256 bytes")
	}
	return planTrackedArchive(existing, incoming, opts)
}

func planTrackedArchive(existing, incoming []archive.Record, opts ArchiveImportOptions) (ArchiveImportPlan, error) {
	var plan ArchiveImportPlan
	destination, err := indexArchiveRecords(existing)
	if err != nil {
		return plan, err
	}
	source, err := indexArchiveRecords(incoming)
	if err != nil {
		return plan, err
	}
	targets, mappings, err := archiveImportTargets(source, destination, opts)
	if err != nil {
		return plan, err
	}
	plan.sources = make(map[archiveRecordKey]archive.Record)
	plan.generatedMappings = make(map[string]bool)
	var pending []archive.Record
	var newMappings []archive.Record
	for _, record := range incoming {
		key := archiveRecordKey{record.Kind, record.ID}
		if record.Kind == "import-mappings" {
			// Imported provenance describes the original destination. Do not silently
			// change its meaning when copying those records to another identity.
			saved := source[key].(*lobslawv1.ArchiveMapping)
			targetKey := archiveRecordKey{saved.Kind, saved.DestinationId}
			if target, ok := targets[targetKey]; ok && target != saved.DestinationId {
				return plan, errors.New("cannot remap a record with imported provenance; restore it without alongside or owner remapping")
			}
			pending = append(pending, record)
			plan.sources[key] = record
			continue
		}
		msg := proto.Clone(source[key])
		if _, err := mapArchiveOwner(record.ID, msg, opts.Owners); err != nil {
			return plan, err
		}
		if err := remapArchiveReferences(msg, key, targets); err != nil {
			return plan, err
		}
		originalDigest, err := archiveFingerprint(msg)
		if err != nil {
			return plan, err
		}
		paused := proto.Clone(msg)
		if _, err := pauseArchiveRecord(record.Kind, paused, opts.SourceTimezone); err != nil {
			return plan, err
		}
		targetID := targets[key]
		targetKey := archiveRecordKey{record.Kind, targetID}
		targetDigest, err := archiveFingerprint(paused)
		if err != nil {
			return plan, err
		}
		// The source baseline includes owner/reference mappings and execution
		// preparation, without treating a matching existing active copy as an edit.
		baseline, err := json.Marshal([]string{originalDigest, targetDigest})
		if err != nil {
			return plan, err
		}
		sourceDigest := Digest(baseline)
		current := destination[targetKey]
		currentDigest := ""
		if current != nil {
			currentDigest, err = archiveFingerprint(current)
			if err != nil {
				return plan, err
			}
		}
		if saved := mappings[key]; saved != nil {
			reason := archiveMappingConflict(current, currentDigest, sourceDigest, saved)
			if reason != "" {
				plan.Conflicts = append(plan.Conflicts, ArchiveRecordRef{Kind: record.Kind, ID: record.ID})
				plan.ConflictDetails = append(plan.ConflictDetails, ArchiveConflict{Kind: record.Kind, ID: record.ID, DestinationID: targetID, Reason: reason})
			} else {
				plan.Duplicates = append(plan.Duplicates, ArchiveRecordRef{Kind: record.Kind, ID: record.ID})
			}
			continue
		}
		raw, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(msg)
		if err != nil {
			return plan, err
		}
		transformed := archive.Record{Kind: record.Kind, ID: targetID, Data: raw}
		pending = append(pending, transformed)
		plan.sources[targetKey] = record
		// Existing identical records can be adopted, but differing records remain
		// conflicts until the operator explicitly chooses alongside or skip.
		if current != nil && currentDigest != targetDigest && currentDigest != originalDigest {
			continue
		}
		if current != nil {
			targetDigest = currentDigest
		}
		mapping := &lobslawv1.ArchiveMapping{
			Id: archiveMappingID(opts.SourceID, record.Kind, record.ID), SourceId: opts.SourceID,
			Kind: record.Kind, SourceRecordId: record.ID, DestinationId: targetID,
			SourceDigest: sourceDigest, DestinationDigest: targetDigest,
		}
		raw, err = (protojson.MarshalOptions{UseProtoNames: true}).Marshal(mapping)
		if err != nil {
			return plan, err
		}
		if _, exists := source[archiveRecordKey{"import-mappings", mapping.Id}]; exists {
			return plan, errors.New("source identity collides with imported provenance; restore the archive instead")
		}
		newMappings = append(newMappings, archive.Record{Kind: "import-mappings", ID: mapping.Id, Data: raw})
		plan.generatedMappings[mapping.Id] = true
	}
	// Ownership and pausing were handled above. Use identity mappings for the
	// ordinary planner so it does not reinterpret already mapped principals.
	mappedOpts := opts
	mappedOpts.Owners = make(map[string]string)
	for _, owner := range opts.Owners {
		mappedOpts.Owners[owner] = owner
	}
	// Preserve conflict previews when mapped deletions invalidate dependencies.
	if len(plan.Conflicts) != 0 {
		return plan, nil
	}
	base, err := planArchiveRecords(existing, pending, mappedOpts)
	if err != nil {
		return plan, err
	}
	plan.Added = base.Added
	plan.Additions = base.Additions
	plan.Paused = base.Paused
	plan.Embeddings = base.Embeddings
	plan.Duplicates = append(plan.Duplicates, base.Duplicates...)
	plan.Conflicts = append(plan.Conflicts, base.Conflicts...)
	for _, mapping := range newMappings {
		plan.Additions = append(plan.Additions, mapping)
		plan.sources[archiveRecordKey{mapping.Kind, mapping.ID}] = mapping
	}
	return plan, nil
}

func archiveMappingConflict(current proto.Message, currentDigest, sourceDigest string, saved *lobslawv1.ArchiveMapping) string {
	switch {
	case current == nil:
		return "destination deleted"
	case currentDigest != saved.DestinationDigest:
		return "destination changed"
	case sourceDigest != saved.SourceDigest:
		return "source or import options changed"
	default:
		return ""
	}
}

func archiveImportTargets(source, destination map[archiveRecordKey]proto.Message, opts ArchiveImportOptions) (map[archiveRecordKey]string, map[archiveRecordKey]*lobslawv1.ArchiveMapping, error) {
	selected := make(map[archiveRecordKey]bool)
	for _, ref := range opts.Alongside {
		key := archiveRecordKey{ref.Kind, ref.ID}
		if selected[key] {
			return nil, nil, fmt.Errorf("duplicate alongside selection %s/%s", ref.Kind, ref.ID)
		}
		if _, ok := source[key]; !ok {
			return nil, nil, fmt.Errorf("alongside selection %s/%s is not in the archive", ref.Kind, ref.ID)
		}
		switch ref.Kind {
		case "sessions", "documents", "episodic", "consolidations", "scheduled-tasks", "commitments":
		default:
			return nil, nil, fmt.Errorf("alongside is not supported for %s; session messages must follow their session", ref.Kind)
		}
		selected[key] = true
	}
	targets := make(map[archiveRecordKey]string)
	mappings := make(map[archiveRecordKey]*lobslawv1.ArchiveMapping)
	for key, msg := range source {
		if key.kind == "import-mappings" {
			continue
		}
		id, err := mapArchiveOwner(key.id, proto.Clone(msg), opts.Owners)
		if err != nil {
			return nil, nil, err
		}
		mappingID := archiveMappingID(opts.SourceID, key.kind, key.id)
		if saved, ok := destination[archiveRecordKey{"import-mappings", mappingID}]; ok {
			mapping := saved.(*lobslawv1.ArchiveMapping)
			mappings[key] = mapping
			id = mapping.DestinationId
		} else if selected[key] {
			id = "import:" + mappingID
		}
		if override, ok := opts.targets[key]; ok {
			id = override
		}
		targets[key] = id
	}
	// Session message keys include their parent's ID. Select the parent once;
	// every message, including messages added in later generations, follows it.
	for key, msg := range source {
		if rec, ok := msg.(*lobslawv1.SessionMessage); ok {
			if id, exists := targets[archiveRecordKey{"sessions", rec.SessionId}]; exists {
				target := sessionMessageKey(id, rec.Seq)
				if saved := mappings[key]; saved != nil && saved.DestinationId != target {
					return nil, nil, errors.New("saved transcript mapping disagrees with its session")
				}
				targets[key] = target
			}
		}
	}
	return targets, mappings, nil
}

func remapArchiveReferences(msg proto.Message, key archiveRecordKey, targets map[archiveRecordKey]string) error {
	m := msg.ProtoReflect()
	fields := m.Descriptor().Fields()
	if inbox, ok := msg.(*lobslawv1.BotInboxItem); ok {
		id, matches := strings.CutPrefix(targets[key], inbox.GetRecipient()+":")
		if !matches || id == "" {
			return errors.New("inbox destination key disagrees with recipient")
		}
		inbox.Id = id
	} else if f := fields.ByName("id"); f != nil {
		m.Set(f, protoreflect.ValueOfString(targets[key]))
	}
	for _, name := range []protoreflect.Name{"session_id", "parent_id"} {
		if f := fields.ByName(name); f != nil {
			old := m.Get(f).String()
			if target, ok := targets[archiveRecordKey{"sessions", old}]; ok {
				m.Set(f, protoreflect.ValueOfString(target))
			}
		}
	}
	if f := fields.ByName("source_ids"); f != nil {
		list := m.Mutable(f).List()
		for i := 0; i < list.Len(); i++ {
			old := list.Get(i).String()
			replacement := ""
			for _, kind := range []string{"episodic", "documents", "consolidations"} {
				if target, ok := targets[archiveRecordKey{kind, old}]; ok && target != old {
					if replacement != "" && replacement != target {
						return errors.New("ambiguous remapped provenance reference")
					}
					replacement = target
				}
			}
			if replacement != "" {
				list.Set(i, protoreflect.ValueOfString(replacement))
			}
		}
	}
	return nil
}

func skipArchiveRecords(incoming []archive.Record, opts ArchiveImportOptions) ([]archive.Record, []ArchiveRecordRef, error) {
	index, err := indexArchiveRecords(incoming)
	if err != nil {
		return nil, nil, err
	}
	selected := make(map[archiveRecordKey]bool)
	for _, ref := range opts.Skip {
		key := archiveRecordKey{ref.Kind, ref.ID}
		if _, ok := index[key]; !ok {
			return nil, nil, fmt.Errorf("skip selection %s/%s is not in the archive", ref.Kind, ref.ID)
		}
		if ref.Kind == "session-messages" || ref.Kind == "import-mappings" {
			return nil, nil, errors.New("skip the complete session or record group, not individual messages or provenance")
		}
		if selected[key] {
			return nil, nil, errors.New("duplicate skip selection")
		}
		selected[key] = true
	}
	for _, ref := range opts.Alongside {
		if selected[archiveRecordKey{ref.Kind, ref.ID}] {
			return nil, nil, errors.New("record cannot be both skipped and imported alongside")
		}
	}
	var records []archive.Record
	var skipped []ArchiveRecordRef
	for _, record := range incoming {
		key := archiveRecordKey{record.Kind, record.ID}
		skip := selected[key]
		if msg, ok := index[key].(*lobslawv1.SessionMessage); ok {
			skip = selected[archiveRecordKey{"sessions", msg.SessionId}]
		}
		if mapping, ok := index[key].(*lobslawv1.ArchiveMapping); ok {
			skip = selected[archiveRecordKey{mapping.Kind, mapping.DestinationId}]
			if mapping.Kind == "session-messages" {
				if split := strings.LastIndex(mapping.DestinationId, ":"); split > 0 {
					skip = selected[archiveRecordKey{"sessions", mapping.DestinationId[:split]}]
				}
			}
		}
		if skip {
			skipped = append(skipped, ArchiveRecordRef{Kind: record.Kind, ID: record.ID})
		} else {
			records = append(records, record)
		}
	}
	return records, skipped, nil
}
