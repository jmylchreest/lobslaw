package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/archive"
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
	if len(plan.Conflicts) > 0 && !opts.KeepExisting {
		return result, fmt.Errorf("archive import has %d conflicts; no records written", len(plan.Conflicts))
	}
	result.Completed += len(plan.Duplicates) + len(plan.Conflicts)
	if plan.Embeddings > 0 && (embedder == nil || embedder.Model() == "") {
		return result, errors.New("destination embedder unavailable; import can be resumed")
	}
	if plan.Embeddings > 0 {
		if err := checkArchiveEmbeddingModel(store, embedder.Model()); err != nil {
			return result, err
		}
	}
	groups, err := archiveImportGroups(plan.Additions)
	if err != nil {
		return result, err
	}
	sources, err := archiveMappedSources(pending, opts)
	if err != nil {
		return result, err
	}
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		batch, err := prepareArchiveBatch(ctx, store, id, group, sources, embedder)
		if err != nil {
			return result, err
		}
		entry := putEntry(batch.BatchId, &lobslawv1.LogEntry{
			Payload: &lobslawv1.LogEntry_ArchiveBatch{ArchiveBatch: batch},
		})
		data, err := proto.Marshal(entry)
		if err != nil {
			return result, err
		}
		response, err := raft.Apply(data, 30*time.Second)
		if err != nil {
			return result, err
		}
		if applyErr, ok := response.(error); ok && applyErr != nil {
			return result, applyErr
		}
		result.Applied += len(group)
		result.Completed += len(group)
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
