package memory

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/logging"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// applyTimeout bounds raft.Apply waits. Healthy small clusters commit
// well under a second; a longer bound absorbs slow-disk hiccups
// without timing out legitimate writes.
const applyTimeout = 5 * time.Second

// Service implements lobslawv1.MemoryServiceServer. Writes go through
// raft.Apply; reads go directly to the local Store. Search is a pure
// in-process linear scan for MVP.
type Service struct {
	lobslawv1.UnimplementedMemoryServiceServer

	raft        *RaftNode
	store       *Store
	logger      *slog.Logger
	dreamRunner *DreamRunner
}

// NewService wires a MemoryService against an existing Raft stack.
// When raft is non-nil, a DreamRunner is constructed alongside — wire
// a Summarizer on it (Phase 5) to enable consolidation; until then
// dream runs score + prune but skip the consolidation step.
func NewService(raft *RaftNode, store *Store, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{raft: raft, store: store, logger: logger}
	if raft != nil {
		s.dreamRunner = NewDreamRunner(raft, store, nil, DreamConfig{}, logger)
	}
	return s
}

// DreamRunner exposes the runner so Phase 5 can inject a Summarizer
// (via DreamRunner.SetSummarizer). Returns nil on nodes without
// raft (compute-only, gateway-only).
func (s *Service) DreamRunner() *DreamRunner { return s.dreamRunner }

// Store persists a VectorRecord through Raft. Writes must run on the
// leader — followers return FailedPrecondition with the leader's address
// so callers can retry.
func (s *Service) Store(ctx context.Context, req *lobslawv1.StoreRequest) (*lobslawv1.StoreResponse, error) {
	if req == nil || req.Record == nil {
		return nil, status.Error(codes.InvalidArgument, "record required")
	}
	rec := req.Record
	if rec.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "record.id required")
	}
	if rec.Retention == "" {
		rec.Retention = string(types.RetentionEpisodic)
	}
	if rec.CreatedAt == nil {
		rec.CreatedAt = timestamppb.Now()
	}
	if err := s.applyEntry(&lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT,
		Id: rec.Id,
		Payload: &lobslawv1.LogEntry_VectorRecord{
			VectorRecord: rec,
		},
	}); err != nil {
		return nil, err
	}
	logging.From(ctx).Debug("vector record stored", "id", rec.Id, "scope", rec.Scope, "retention", rec.Retention)
	return &lobslawv1.StoreResponse{Id: rec.Id}, nil
}

// Recall reads a single VectorRecord by id. Runs locally — no Raft
// round-trip. Returns NotFound if the record isn't present.
func (s *Service) Recall(ctx context.Context, req *lobslawv1.RecallRequest) (*lobslawv1.RecallResponse, error) {
	if req == nil || req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id required")
	}
	raw, err := s.store.Get(BucketVectorRecords, req.Id)
	if err != nil {
		if errors.Is(err, types.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "vector record %q not found", req.Id)
		}
		return nil, status.Errorf(codes.Internal, "store: %v", err)
	}
	var rec lobslawv1.VectorRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, status.Errorf(codes.Internal, "unmarshal: %v", err)
	}
	return &lobslawv1.RecallResponse{Record: &rec}, nil
}

// Search performs vector similarity search over the local store.
// Required: pre-computed Embedding. The Text field is reserved for
// Phase 5 (when the Provider Resolver can supply embeddings) and
// returns Unimplemented until then.
func (s *Service) Search(ctx context.Context, req *lobslawv1.SearchRequest) (*lobslawv1.SearchResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	if len(req.Embedding) == 0 {
		if req.Text != "" {
			return nil, status.Error(codes.Unimplemented,
				"text→embedding resolution not wired yet; supply req.embedding directly")
		}
		return nil, status.Error(codes.InvalidArgument, "embedding required")
	}
	hits, err := vectorSearch(s.store, req.Embedding, int(req.Limit), req.ScopeFilter, req.RetentionFilter)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "search: %v", err)
	}
	out := make([]*lobslawv1.VectorRecord, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.record)
	}
	logging.From(ctx).Debug("vector search", "query_dim", len(req.Embedding), "hits", len(out))
	return &lobslawv1.SearchResponse{Hits: out}, nil
}

// EpisodicAdd records a single EpisodicRecord through Raft.
func (s *Service) EpisodicAdd(ctx context.Context, req *lobslawv1.EpisodicAddRequest) (*lobslawv1.EpisodicAddResponse, error) {
	if req == nil || req.Record == nil {
		return nil, status.Error(codes.InvalidArgument, "record required")
	}
	rec := req.Record
	if rec.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "record.id required")
	}
	if rec.Retention == "" {
		rec.Retention = string(types.RetentionEpisodic)
	}
	if rec.Timestamp == nil {
		rec.Timestamp = timestamppb.Now()
	}
	if rec.Importance == 0 {
		// Default to mid-range — dream consolidation scores by
		// (importance × recency × access_freq); zero importance would
		// silently exclude the record.
		rec.Importance = 5
	}
	if err := s.applyEntry(&lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT,
		Id: rec.Id,
		Payload: &lobslawv1.LogEntry_EpisodicRecord{
			EpisodicRecord: rec,
		},
	}); err != nil {
		return nil, err
	}
	logging.From(ctx).Debug("episodic record added", "id", rec.Id, "importance", rec.Importance)
	return &lobslawv1.EpisodicAddResponse{Id: rec.Id}, nil
}

// Dream triggers one Dream/REM consolidation pass. Leader-only —
// followers soft-skip with FailedPrecondition. When no Summarizer
// is wired the pass still runs (score + prune + session log), but
// Consolidated in the response will be 0.
func (s *Service) Dream(ctx context.Context, _ *lobslawv1.DreamRequest) (*lobslawv1.DreamResponse, error) {
	if s.dreamRunner == nil {
		return nil, status.Error(codes.Unimplemented, "raft stack not wired on this node")
	}
	if s.raft != nil && !s.raft.IsLeader() {
		return nil, status.Errorf(codes.FailedPrecondition,
			"not the raft leader; retry at %s", s.raft.LeaderAddress())
	}
	result, err := s.dreamRunner.Run(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "dream: %v", err)
	}
	if result == nil {
		return nil, status.Error(codes.FailedPrecondition, "not the raft leader")
	}
	return &lobslawv1.DreamResponse{
		Consolidated: int32(result.Consolidated),
		Pruned:       int32(result.Pruned),
	}, nil
}

// Forget deletes source records matching the query, then cascades to
// any consolidated records whose sources intersect with the forgotten
// set. Aggressive by design — a summary that "remembers" a forgotten
// source still leaks its content, so we sweep it too.
//
// Each deletion goes through Raft as a LogEntry{DELETE}. Requires
// leadership; followers return FailedPrecondition.
func (s *Service) Forget(ctx context.Context, req *lobslawv1.ForgetRequest) (*lobslawv1.ForgetResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	if req.Query == "" && req.Before == nil && len(req.Tags) == 0 {
		return nil, status.Error(codes.InvalidArgument,
			"at least one filter (query, before, tags) required — refusing to forget everything")
	}
	if s.raft != nil && !s.raft.IsLeader() {
		return nil, status.Errorf(codes.FailedPrecondition,
			"not the raft leader; retry at %s", s.raft.LeaderAddress())
	}

	var before time.Time
	if req.Before != nil {
		before = req.Before.AsTime()
	}

	matched, err := forgetScan(s.store, req.Query, before, req.Tags)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "forget scan: %v", err)
	}

	swept, err := forgetCascade(s.store, matched)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "forget cascade: %v", err)
	}

	// Delete each matched and swept record through Raft. We don't know
	// the bucket of each id at this layer; the FSM-level dispatch
	// requires the record type, so we issue deletes with a VectorRecord
	// payload stub as the type discriminator. The actual bucket is
	// determined from the SourceIDs we already saw — but both buckets
	// use the same-id-space so we try both for robustness.
	for id := range matched {
		if err := s.deleteFromBothBuckets(id); err != nil {
			return nil, status.Errorf(codes.Internal, "delete %q: %v", id, err)
		}
	}
	for id := range swept {
		if err := s.deleteFromBothBuckets(id); err != nil {
			return nil, status.Errorf(codes.Internal, "delete cascade %q: %v", id, err)
		}
	}

	logging.From(ctx).Info("memory forget",
		"query", req.Query,
		"before", before,
		"tags", req.Tags,
		"direct", len(matched),
		"cascaded", len(swept),
	)

	return &lobslawv1.ForgetResponse{
		RecordsRemoved:         int32(len(matched)),
		ConsolidationsReforged: int32(len(swept)),
	}, nil
}

// deleteFromBothBuckets issues a DELETE log entry against both
// VectorRecord and EpisodicRecord buckets. The FSM's applyDelete is
// idempotent for absent keys, so the entry for whichever bucket
// doesn't hold the id is a cheap no-op.
func (s *Service) deleteFromBothBuckets(id string) error {
	for _, payload := range []*lobslawv1.LogEntry{
		{
			Op:      lobslawv1.LogOp_LOG_OP_DELETE,
			Id:      id,
			Payload: &lobslawv1.LogEntry_VectorRecord{VectorRecord: &lobslawv1.VectorRecord{Id: id}},
		},
		{
			Op:      lobslawv1.LogOp_LOG_OP_DELETE,
			Id:      id,
			Payload: &lobslawv1.LogEntry_EpisodicRecord{EpisodicRecord: &lobslawv1.EpisodicRecord{Id: id}},
		},
	} {
		if err := s.applyEntry(payload); err != nil {
			return err
		}
	}
	return nil
}

// applyEntry proto-marshals e and submits it to Raft. Followers get a
// FailedPrecondition with the leader's address; callers retry there.
func (s *Service) applyEntry(e *lobslawv1.LogEntry) error {
	if s.raft == nil {
		return status.Error(codes.Unimplemented, "raft stack not wired on this node")
	}
	if !s.raft.IsLeader() {
		return status.Errorf(codes.FailedPrecondition,
			"not the raft leader; retry at %s", s.raft.LeaderAddress())
	}
	data, err := proto.Marshal(e)
	if err != nil {
		return status.Errorf(codes.Internal, "marshal log entry: %v", err)
	}
	resp, err := s.raft.Apply(data, applyTimeout)
	if err != nil {
		return status.Errorf(codes.Internal, "raft apply: %v", err)
	}
	// FSM.Apply can return a plain error; surface it.
	if fsmErr, ok := resp.(error); ok && fsmErr != nil {
		return status.Errorf(codes.Internal, "fsm apply: %v", fsmErr)
	}
	return nil
}
