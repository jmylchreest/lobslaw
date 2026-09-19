package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/jmylchreest/lobslaw/internal/archive"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// ArchiveImportOptions resolves identities explicitly, including when the
// operator has established that a source identity means the same person here.
// It is not authorization: the caller must authorize destination data access.
type ArchiveImportOptions struct {
	ReplaceOriginal []ArchiveRecordRef `json:"replace_original,omitempty"`
	Replace         []ArchiveRecordRef `json:"replace,omitempty"`
	BackupDigest    string             `json:"backup_digest,omitempty"`
	targets         map[archiveRecordKey]string

	Skip           []ArchiveRecordRef `json:"skip,omitempty"`
	SourceID       string             `json:"source_id,omitempty"`
	Alongside      []ArchiveRecordRef `json:"alongside,omitempty"`
	Owners         map[string]string  `json:"owners"`
	SourceTimezone string             `json:"source_timezone"`
	KeepExisting   bool               `json:"keep_existing"`
}

type ArchiveRecordRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// ArchiveImportPlan contains content only in Additions. A CLI should print the
// inventory fields, rather than inadvertently print memories with its preview.
// A plan is a preview, not a write capability; apply must check current state.
type ArchiveImportPlan struct {
	Replaced []ArchiveRecordRef `json:"replaced,omitempty"`
	// Removed includes overwritten records and retired records, including provenance.
	Removed []ArchiveRecordRef `json:"removed,omitempty"`

	Skipped           []ArchiveRecordRef `json:"skipped,omitempty"`
	sources           map[archiveRecordKey]archive.Record
	generatedMappings map[string]bool
	ConflictDetails   []ArchiveConflict  `json:"conflict_details,omitempty"`
	Added             []ArchiveRecordRef `json:"additions"`
	Additions         []archive.Record   `json:"-"`
	Duplicates        []ArchiveRecordRef `json:"duplicates"`
	Conflicts         []ArchiveRecordRef `json:"conflicts"`
	Paused            []ArchiveRecordRef `json:"paused"`
	Embeddings        int                `json:"embeddings"`
}

type archiveRecordKey struct {
	kind string
	id   string
}

// PlanArchiveImport operates on already verified, consistently read snapshots.
// It never modifies either input and does not contact the embedder. Historical
// provenance can reference retired records; executable blobs and transcript
// parents, in contrast, must exist in the effective destination.
func planArchiveRecords(existing, incoming []archive.Record, opts ArchiveImportOptions) (ArchiveImportPlan, error) {
	var plan ArchiveImportPlan
	destination, err := indexArchiveRecords(existing)
	if err != nil {
		return plan, fmt.Errorf("destination: %w", err)
	}
	if _, err := indexArchiveRecords(incoming); err != nil {
		return plan, err
	}
	seen := make(map[archiveRecordKey]bool)
	for _, record := range incoming {
		msg, err := decodeArchiveRecord(record)
		if err != nil {
			return plan, err
		}
		id, err := mapArchiveOwner(record.ID, msg, opts.Owners)
		if err != nil {
			return plan, fmt.Errorf("%s/%s: %w", record.Kind, record.ID, err)
		}
		original := proto.Clone(msg)
		paused, err := pauseArchiveRecord(record.Kind, msg, opts.SourceTimezone)
		if err != nil {
			return plan, fmt.Errorf("%s/%s: %w", record.Kind, record.ID, err)
		}
		key := archiveRecordKey{record.Kind, id}
		if seen[key] {
			return plan, fmt.Errorf("ownership mappings collide at %s/%s", key.kind, key.id)
		}
		seen[key] = true
		ref := ArchiveRecordRef{Kind: key.kind, ID: key.id}
		if current, ok := destination[key]; ok {
			if proto.Equal(current, msg) || proto.Equal(current, original) {
				plan.Duplicates = append(plan.Duplicates, ref)
			} else {
				plan.Conflicts = append(plan.Conflicts, ref)
			}
			continue
		}
		data, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(msg)
		if err != nil {
			return plan, err
		}
		plan.Added = append(plan.Added, ref)
		plan.Additions = append(plan.Additions, archive.Record{Kind: key.kind, ID: key.id, Data: data})
		destination[key] = msg
		if paused {
			plan.Paused = append(plan.Paused, ref)
		}
		if record.Kind == "documents" || record.Kind == "learned" {
			plan.Embeddings++
		}
	}
	// Unresolved conflicts can change dependency validity, such as when two
	// session indexes cover different transcript ranges. Report those conflicts
	// first; apply refuses them before writing. Keeping existing records makes
	// the destination concrete, so its dependencies must pass validation.
	if len(plan.Conflicts) == 0 || opts.KeepExisting {
		if err := validateArchiveDependencies(destination); err != nil {
			return plan, err
		}
	}
	sort.Slice(plan.Additions, func(i, j int) bool {
		left, right := plan.Additions[i], plan.Additions[j]
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.ID < right.ID
	})
	return plan, nil
}

func indexArchiveRecords(records []archive.Record) (map[archiveRecordKey]proto.Message, error) {
	index := make(map[archiveRecordKey]proto.Message, len(records))
	for _, record := range records {
		key := archiveRecordKey{record.Kind, record.ID}
		if _, exists := index[key]; exists {
			return nil, fmt.Errorf("duplicate record %s/%s", record.Kind, record.ID)
		}
		msg, err := decodeArchiveRecord(record)
		if err != nil {
			return nil, err
		}
		index[key] = msg
	}
	return index, nil
}

func decodeArchiveRecord(record archive.Record) (proto.Message, error) {
	for _, kind := range archiveKinds {
		if kind.kind != record.Kind {
			continue
		}
		msg := proto.Clone(kind.message)
		if err := protojson.Unmarshal(record.Data, msg); err != nil {
			return nil, fmt.Errorf("decode %s/%s: invalid or unsupported record fields", record.Kind, record.ID)
		}
		if err := portableMessage(msg.ProtoReflect()); err != nil {
			return nil, err
		}
		if err := validateArchiveRecordID(record, msg); err != nil {
			return nil, err
		}
		return msg, nil
	}
	return nil, fmt.Errorf("unsupported archive record kind %q", record.Kind)
}

func validateArchiveRecordID(record archive.Record, msg proto.Message) error {
	if record.ID == "" {
		return errors.New("archive record has an empty id")
	}
	id := record.ID
	switch rec := msg.(type) {
	case *lobslawv1.ArchiveMapping:
		if rec.SourceId == "" || rec.Kind == "import-mappings" || rec.DestinationId == "" || rec.SourceRecordId == "" || rec.Id != archiveMappingID(rec.SourceId, rec.Kind, rec.SourceRecordId) {
			return errors.New("invalid import mapping identity")
		}
		if _, err := findArchiveKind(rec.Kind); err != nil {
			return err
		}
		for _, digest := range []string{rec.SourceDigest, rec.DestinationDigest} {
			if len(digest) != len("sha256:")+64 || !strings.HasPrefix(digest, "sha256:") {
				return errors.New("invalid import mapping fingerprint")
			}
		}
		id = rec.Id
	case *lobslawv1.SkillRecord:
		id = SkillKey(rec.Name, rec.Version)
		if rec.Name == "" || rec.Version == "" {
			return errors.New("skill requires name and version")
		}
	case *lobslawv1.SkillBlob:
		id = rec.Digest
		if Digest(rec.Content) != id || len(rec.Content) > DefaultMaxSkillFileBytes {
			return errors.New("skill blob content does not match digest or exceeds size limit")
		}
	case *lobslawv1.SessionMessage:
		id = sessionMessageKey(rec.SessionId, rec.Seq)
	case *lobslawv1.BotInboxItem:
		id = inboxKey(rec.Recipient, rec.Id)
	case *lobslawv1.UserPreferences:
		id = rec.UserId
	default:
		fields := msg.ProtoReflect().Descriptor().Fields()
		if field := fields.ByName("id"); field != nil {
			id = msg.ProtoReflect().Get(field).String()
		}
	}
	if id != record.ID {
		return fmt.Errorf("%s/%s: record id disagrees with payload", record.Kind, record.ID)
	}
	return nil
}

func mapArchiveOwner(id string, msg proto.Message, owners map[string]string) (string, error) {
	value := msg.ProtoReflect()
	for _, name := range []protoreflect.Name{"owner", "user_id"} {
		field := value.Descriptor().Fields().ByName(name)
		if field == nil {
			continue
		}
		owner := value.Get(field).String()
		// Preserve unowned records as unowned, never as shared or as the
		// importing operator's data. Repairing source ownership is separate.
		if owner == "" {
			continue
		}
		next, ok := owners[owner]
		if !ok || strings.TrimSpace(next) == "" {
			return "", fmt.Errorf("explicit owner mapping required for %q", owner)
		}
		value.Set(field, protoreflect.ValueOfString(next))
	}
	switch rec := msg.(type) {
	case *lobslawv1.UserPreferences:
		id = rec.UserId
	case *lobslawv1.PinnedMemory:
		if rec.Kind != "profile" && rec.Kind != "notes" {
			return "", errors.New("invalid pinned memory kind")
		}
		rec.Id = rec.Kind + ":" + rec.UserId
		id = rec.Id
	}
	return id, nil
}

func pauseArchiveRecord(kind string, msg proto.Message, timezone string) (bool, error) {
	switch rec := msg.(type) {
	case *lobslawv1.BotInboxItem:
		if rec.Status == lobslawv1.InboxStatus_INBOX_STATUS_PENDING || rec.Status == lobslawv1.InboxStatus_INBOX_STATUS_CLAIMED {
			rec.Status = lobslawv1.InboxStatus_INBOX_STATUS_CANCELLED
			rec.ClaimedBy, rec.ClaimExpiresAt = "", nil
			rec.Error = "paused by archive import; retry explicitly to resume"
			return true, nil
		}
	case *lobslawv1.ScheduledTaskRecord:
		if !strings.HasPrefix(rec.Schedule, "CRON_TZ=") && !strings.HasPrefix(rec.Schedule, "TZ=") {
			if timezone == "" {
				return false, errors.New("source timezone is required for cron schedules")
			}
			if _, err := time.LoadLocation(timezone); err != nil {
				return false, fmt.Errorf("source timezone: %w", err)
			}
			rec.Schedule = "CRON_TZ=" + timezone + " " + rec.Schedule
		}
		if _, err := cron.ParseStandard(rec.Schedule); err != nil {
			return false, fmt.Errorf("cron expression: %w", err)
		}
		paused := rec.Enabled
		rec.Enabled = false
		rec.NextRun = nil
		return paused, nil
	case *lobslawv1.AgentCommitment:
		switch rec.Status {
		case "pending":
			rec.Status = "paused"
			return true, nil
		case "paused", "done", "cancelled":
			return false, nil
		default:
			return false, errors.New("unknown commitment status")
		}
	case *lobslawv1.SkillRecord:
		paused := rec.Active
		rec.Active = false
		return paused, nil
	case *lobslawv1.SelfTaughtRecord:
		if kind == "learned" && (rec.State == lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_ACTIVE || rec.State == lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_STALE) {
			rec.State = lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_PROPOSED
			return true, nil
		}
	case *lobslawv1.UserPreferences:
		paused := len(rec.Channels) > 0
		rec.Channels = nil
		return paused, nil
	}
	return false, nil
}

func validateArchiveDependencies(records map[archiveRecordKey]proto.Message) error {
	for key, msg := range records {
		switch rec := msg.(type) {
		case *lobslawv1.SelfTaughtRecord:
			for _, files := range []map[string]string{rec.Files, rec.GetPending().GetFiles()} {
				for path := range files {
					if !fs.ValidPath(path) || path == "." || strings.Contains(path, "\\") {
						return fmt.Errorf("learned artefact %s has unsafe payload path", key.id)
					}
				}
			}
		case *lobslawv1.SkillRecord:
			total := len(rec.ManifestYaml) + len(rec.ManifestSig)
			for path, digest := range rec.Files {
				if !fs.ValidPath(path) || path == "." || path == ManifestFile || path == SignatureFile || strings.Contains(path, "\\") {
					return fmt.Errorf("skill %s has unsafe payload path", key.id)
				}
				blob, ok := records[archiveRecordKey{"skill-blobs", digest}].(*lobslawv1.SkillBlob)
				if !ok {
					return fmt.Errorf("skill %s requires missing blob %s", key.id, digest)
				}
				total += len(blob.Content)
			}
			if total > DefaultMaxSkillTotalBytes {
				return ErrSkillTooLarge
			}
		case *lobslawv1.SessionMessage:
			session, ok := records[archiveRecordKey{"sessions", rec.SessionId}].(*lobslawv1.SessionRecord)
			if !ok || rec.Seq < session.FirstSeq || rec.Seq >= session.NextSeq {
				return fmt.Errorf("message %s has missing session or invalid sequence", key.id)
			}
		}
	}
	return nil
}
