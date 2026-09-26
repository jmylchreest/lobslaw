package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/archive"
	"github.com/jmylchreest/lobslaw/internal/sharing"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type ArchiveImportResult struct {
	ImportID  string `json:"import_id"`
	Applied   int    `json:"applied"`
	Completed int    `json:"completed"`
	Total     int    `json:"total"`
}

// The source records retain requested activation and channel bindings. They
// remain encrypted in the import journal and never confer destination authority.
type archiveReceipt struct {
	Sources []archive.Record `json:"sources"`
}

// ApplyArchiveImport revalidates the whole pending import before writing. Each
// completed batch is durable; returning an error includes the completed count.
// Resume re-supplies the same archive and options, rather than retaining an
// unbounded upload in the node's filesystem.
func ApplyArchiveImport(ctx context.Context, raft RebindApplier, store *Store, incoming []archive.Record, opts ArchiveImportOptions, embedder ReembedEmbedder) (ArchiveImportResult, error) {
	result := ArchiveImportResult{Total: len(incoming)}
	if raft == nil || store == nil {
		return result, errors.New("archive import requires raft and store")
	}
	result, pending, err := archiveImportProgress(store, incoming, opts)
	if err != nil {
		return result, err
	}
	id := result.ImportID
	if len(pending) == 0 {
		return result, nil
	}
	existing, err := store.ArchiveRecords(ctx)
	if err != nil {
		return result, err
	}
	plan, err := PlanArchiveImport(existing, pending, opts)
	if err != nil {
		return result, err
	}
	if err := validateArchiveApply(store, existing, plan, opts, embedder); err != nil {
		return result, err
	}
	result.Completed += len(plan.Duplicates) + len(plan.Conflicts) + len(plan.Skipped)

	groups, err := archiveImportGroups(plan.Additions)
	if err != nil {
		return result, err
	}
	if len(plan.Replaced) > 0 {
		groups = [][]archive.Record{plan.Additions}
	}
	sources := plan.sources
	if sources == nil {
		sources, err = archiveMappedSources(pending, opts)
		if err != nil {
			return result, err
		}
	}
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		batch, err := prepareArchiveBatch(ctx, store, id, group, sources, embedder)
		if err != nil {
			return result, err
		}
		if err := guardArchiveMappings(batch, plan.generatedMappings); err != nil {
			return result, err
		}
		if err := prepareArchiveReplacement(batch, plan, opts.BackupDigest); err != nil {
			return result, err
		}
		entry := putEntry(batch.BatchId, &lobslawv1.LogEntry{
			Payload: &lobslawv1.LogEntry_ArchiveBatch{ArchiveBatch: batch},
		})
		data, err := proto.Marshal(entry)
		if err != nil {
			return result, err
		}
		response, err := raft.Apply(data, archiveApplyTimeout)
		if err != nil {
			return result, err
		}
		if applyErr, ok := response.(error); ok && applyErr != nil {
			return result, applyErr
		}
		for _, record := range group {
			if record.Kind == "import-mappings" && plan.generatedMappings[record.ID] {
				continue
			}
			result.Applied++
			result.Completed++
		}
	}
	return result, nil
}

func checkArchiveEmbeddingModel(store *Store, model string) error {
	return store.ForEach(BucketVectorRecords, func(_ string, raw []byte) error {
		var record lobslawv1.VectorRecord
		if err := proto.Unmarshal(raw, &record); err != nil {
			return err
		}
		if len(record.Embedding) > 0 && record.EmbeddingModel != model {
			return errors.New("destination contains another embedding model; migrate the destination corpus separately before importing")
		}
		return nil
	})
}

func archiveImportID(records []archive.Record, opts ArchiveImportOptions) (string, error) {
	index, err := indexArchiveRecords(records)
	if err != nil {
		return "", err
	}
	var identities []string
	for key, msg := range index {
		raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(msg)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal([]string{key.kind, key.id, Digest(raw)})
		if err != nil {
			return "", err
		}
		identities = append(identities, string(encoded))
	}
	sort.Strings(identities)
	raw, err := json.Marshal(struct {
		Records []string
		Options ArchiveImportOptions
	}{Records: identities, Options: opts})
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(Digest(raw), "sha256:"), nil
}

func archiveCompleted(store *Store, id string) (map[archiveRecordKey]bool, error) {
	completed := make(map[archiveRecordKey]bool)
	err := store.ForEach(BucketArchiveImports, func(key string, raw []byte) error {
		if !strings.HasPrefix(key, id+"/") {
			return nil
		}
		var receipt archiveReceipt
		if err := json.Unmarshal(raw, &receipt); err != nil {
			return err
		}
		for _, record := range receipt.Sources {
			completed[archiveRecordKey{record.Kind, record.ID}] = true
		}
		return nil
	})
	return completed, err
}

func archiveMappedSources(records []archive.Record, opts ArchiveImportOptions) (map[archiveRecordKey]archive.Record, error) {
	sources := make(map[archiveRecordKey]archive.Record)
	for _, record := range records {
		msg, err := decodeArchiveRecord(record)
		if err != nil {
			return nil, err
		}
		id, err := mapArchiveOwner(record.ID, msg, opts.Owners)
		if err != nil {
			return nil, err
		}
		sources[archiveRecordKey{record.Kind, id}] = record
	}
	return sources, nil
}

// A transcript and its index become visible together. Blobs precede skills;
// other records are independent batches, keeping a failed embedding retry small.
func archiveImportGroups(records []archive.Record) ([][]archive.Record, error) {
	groups := make(map[string][]archive.Record)
	for _, record := range records {
		key := "2/" + record.Kind + "/" + record.ID
		switch record.Kind {
		case "import-mappings":
			msg, err := decodeArchiveRecord(record)
			if err != nil {
				return nil, err
			}
			mapping := msg.(*lobslawv1.ArchiveMapping)
			key = "2/" + mapping.Kind + "/" + mapping.DestinationId
			if mapping.Kind == "skill-installations" {
				key = "3/" + mapping.DestinationId
			}
			if mapping.Kind == "skill-blobs" {
				key = "0/" + mapping.DestinationId
			}
			if mapping.Kind == "sessions" {
				key = "1/" + mapping.DestinationId
			}
			if mapping.Kind == "session-messages" {
				// Message keys end in a fixed-width sequence; session IDs can contain colons.
				split := strings.LastIndex(mapping.DestinationId, ":")
				if split <= 0 || len(mapping.DestinationId)-split-1 != sessionSeqWidth {
					return nil, errors.New("invalid mapped message key")
				}
				key = "1/" + mapping.DestinationId[:split]
			}
		case "skill-installations":
			key = "3/" + record.ID
		case "skill-blobs":
			key = "0/" + record.ID
		case "sessions":
			key = "1/" + record.ID
		case "session-messages":
			msg, err := decodeArchiveRecord(record)
			if err != nil {
				return nil, err
			}
			key = "1/" + msg.(*lobslawv1.SessionMessage).SessionId
		}
		groups[key] = append(groups[key], record)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([][]archive.Record, 0, len(keys))
	for _, key := range keys {
		out = append(out, groups[key])
	}
	return out, nil
}

func prepareArchiveBatch(ctx context.Context, store *Store, id string, records []archive.Record, sources map[archiveRecordKey]archive.Record, embedder ReembedEmbedder) (*lobslawv1.ArchiveBatch, error) {
	batch := &lobslawv1.ArchiveBatch{ImportId: id}
	receipt := archiveReceipt{}
	inBatch := make(map[archiveRecordKey]bool)
	// Provenance checks must observe all content writes in the batch.
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].Kind != "import-mappings" && records[j].Kind == "import-mappings"
	})
	for _, record := range records {
		inBatch[archiveRecordKey{record.Kind, record.ID}] = true
	}
	for _, record := range records {
		msg, err := decodeArchiveRecord(record)
		if err != nil {
			return nil, err
		}
		if err := embedArchiveRecord(ctx, record.Kind, msg, embedder); err != nil {
			return nil, err
		}
		raw, err := proto.Marshal(msg)
		if err != nil {
			return nil, err
		}
		batch.Records = append(batch.Records, &lobslawv1.ArchiveMutation{
			Kind: record.Kind, Id: record.ID, Payload: raw,
		})
		receipt.Sources = append(receipt.Sources, sources[archiveRecordKey{record.Kind, record.ID}])
		for _, dependency := range archiveDependencies(msg) {
			if inBatch[dependency] {
				continue
			}
			kind, err := findArchiveKind(dependency.kind)
			if err != nil {
				return nil, err
			}
			raw, err := store.Get(kind.bucket, dependency.id)
			if err != nil {
				return nil, fmt.Errorf("read archive dependency: %w", err)
			}
			batch.Records = append(batch.Records, &lobslawv1.ArchiveMutation{
				Kind: dependency.kind, Id: dependency.id,
				DependencyOnly: true, ExpectedDigest: Digest(raw),
			})
		}
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	batch.Receipt = raw
	batch.BatchId = strings.TrimPrefix(Digest(raw), "sha256:")
	if proto.Size(batch) > maxArchiveBatchBytes {
		return nil, errors.New("dependency-complete archive batch exceeds size limit")
	}
	return batch, nil
}

func archiveDependencies(msg proto.Message) []archiveRecordKey {
	var out []archiveRecordKey
	switch rec := msg.(type) {
	case *lobslawv1.ShareInstallation:
		if a, err := sharing.Decode(rec.Artifact); err == nil {
			out = append(out, archiveRecordKey{"skills", SkillKey(a.Package().Name, a.Package().Version)})
		}
		for _, id := range rec.ScheduleIds {
			out = append(out, archiveRecordKey{"scheduled-tasks", id})
		}
	case *lobslawv1.SkillRecord:
		for _, digest := range rec.Files {
			out = append(out, archiveRecordKey{"skill-blobs", digest})
		}
	case *lobslawv1.SessionMessage:
		out = append(out, archiveRecordKey{"sessions", rec.SessionId})
	}
	return out
}

func embedArchiveRecord(ctx context.Context, kind string, msg proto.Message, embedder ReembedEmbedder) error {
	var text string
	switch rec := msg.(type) {
	case *lobslawv1.VectorRecord:
		text = rec.Text
	case *lobslawv1.SelfTaughtRecord:
		if kind != "learned" {
			return nil
		}
		text = rec.Name + "\n" + rec.Description + "\n" + rec.Body
	default:
		return nil
	}
	vector, err := embedder.Embed(ctx, text)
	if err != nil {
		return fmt.Errorf("destination embedding model %q failed; resume import", embedder.Model())
	}
	if len(vector) == 0 || norm(vector) == 0 {
		return errors.New("destination embedder returned an empty or zero vector")
	}
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return errors.New("destination embedder returned a non-finite vector")
		}
	}
	switch rec := msg.(type) {
	case *lobslawv1.VectorRecord:
		rec.Embedding = vector
		rec.EmbeddingModel = embedder.Model()
	case *lobslawv1.SelfTaughtRecord:
		rec.Embedding = vector
	}
	return nil
}

func archiveImportProgress(store *Store, incoming []archive.Record, opts ArchiveImportOptions) (ArchiveImportResult, []archive.Record, error) {
	result := ArchiveImportResult{Total: len(incoming)}
	id, err := archiveImportID(incoming, opts)
	if err != nil {
		return result, nil, err
	}
	result.ImportID = id
	if opts.SourceID != "" {
		return result, incoming, nil
	}
	completed, err := archiveCompleted(store, id)
	if err != nil {
		return result, nil, err
	}
	var pending []archive.Record
	for _, record := range incoming {
		if completed[archiveRecordKey{record.Kind, record.ID}] {
			result.Completed++
		} else {
			pending = append(pending, record)
		}
	}
	return result, pending, nil
}

func guardArchiveMappings(batch *lobslawv1.ArchiveBatch, generated map[string]bool) error {
	for _, mutation := range batch.Records {
		if mutation.Kind != "import-mappings" || !generated[mutation.Id] {
			continue
		}
		var mapping lobslawv1.ArchiveMapping
		if err := proto.Unmarshal(mutation.Payload, &mapping); err != nil {
			return err
		}
		mutation.ExpectedDigest = mapping.DestinationDigest
	}
	if proto.Size(batch) > maxArchiveBatchBytes {
		return errors.New("dependency-complete archive batch exceeds size limit")
	}
	return nil
}

func validateArchiveApply(store *Store, existing []archive.Record, plan ArchiveImportPlan, opts ArchiveImportOptions, embedder ReembedEmbedder) error {
	if len(plan.Conflicts) > 0 && (!opts.KeepExisting || len(plan.ConflictDetails) > 0) {
		return fmt.Errorf("archive import has %d conflicts; no records written", len(plan.Conflicts))
	}
	if len(plan.Replaced) > 0 {
		digest, err := ArchiveStateDigest(existing)
		if err != nil {
			return err
		}
		if opts.BackupDigest == "" || opts.BackupDigest != digest {
			return errors.New("replacement requires a verified backup of the current destination; export and verify a fresh backup")
		}
	}
	if plan.Embeddings > 0 && (embedder == nil || embedder.Model() == "") {
		return errors.New("destination embedder unavailable; import can be resumed")
	}
	if plan.Embeddings > 0 {
		if err := checkArchiveEmbeddingModel(store, embedder.Model()); err != nil {
			return err
		}
	}
	return nil
}
