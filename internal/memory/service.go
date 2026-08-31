package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/identity"

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

	raft          *RaftNode
	store         *Store
	logger        *slog.Logger
	dreamRunner   *DreamRunner
	sessionPruner *SessionPruner
	embedder      ReembedEmbedder
}

// NewService wires a MemoryService against an existing Raft stack.
// When raft is non-nil, a DreamRunner is constructed alongside with no
// Summarizer: the node wires one in later, and dream runs score +
// prune without one.
func NewService(raft *RaftNode, store *Store, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{raft: raft, store: store, logger: logger}
	if raft != nil {
		s.dreamRunner = NewDreamRunner(raft, store, nil, DreamConfig{}, logger)
		s.sessionPruner = NewSessionPruner(raft, store, SessionPruneConfig{}, logger)
	}
	return s
}

// DreamRunner exposes the runner so the node can inject a Summarizer
// (via DreamRunner.SetSummarizer) once its providers are resolved.
// Returns nil on nodes without raft (compute-only, gateway-only).
func (s *Service) DreamRunner() *DreamRunner { return s.dreamRunner }

// SessionPruner exposes the pruner so node.go can register its
// scheduler handler. Returns nil on raft-less nodes.
func (s *Service) SessionPruner() *SessionPruner { return s.sessionPruner }

// ConfigureSessionPruner replaces the pruner with one tuned by the
// supplied MaxAge. Called from node.go once the operator's
// [memory.session] block has been parsed. Zero MaxAge → default 24h
// (same as NewSessionPruner).
func (s *Service) ConfigureSessionPruner(maxAge time.Duration) {
	if s.raft == nil {
		return
	}
	s.sessionPruner = NewSessionPruner(s.raft, s.store, SessionPruneConfig{MaxAge: maxAge}, s.logger)
}

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
	if rec.Retention == lobslawv1.Retention_RETENTION_UNSPECIFIED {
		rec.Retention = lobslawv1.Retention_RETENTION_EPISODIC
	}
	if rec.CreatedAt == nil {
		rec.CreatedAt = timestamppb.Now()
	}
	if err := s.applyEntry(ctx, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT,
		Id: rec.Id,
		Payload: &lobslawv1.LogEntry_VectorRecord{
			VectorRecord: rec,
		},
	}); err != nil {
		return nil, err
	}
	logging.From(ctx).Debug("vector record stored", "id", rec.Id, "scope", rec.Scope, "retention", types.RetentionString(rec.Retention))
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
// Required: pre-computed Embedding. The Text field returns
// Unimplemented: embedding belongs to the caller, which already holds
// the provider resolver, so doing it here would put a provider call
// behind a store read.
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
	// Everyone() because MemoryService.Search is the cluster RPC: it is
	// reached over mTLS by peer nodes and operator tooling, not by a
	// model, and the request carries no principal to scope against.
	// Spelled out rather than defaulted so that when the RPC does grow
	// an identity field, this is the line that has to change.
	hits, err := vectorSearch(s.store, req.Embedding, int(req.Limit), Everyone(), req.ScopeFilter, req.RetentionFilter)
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

// FindClusters returns connected components of vector records
// linked by pairwise cosine similarity above the threshold.
// Deterministic (no LLM): it reports which records look alike and
// decides nothing about them. Runs against the local store.
func (s *Service) FindClusters(ctx context.Context, req *lobslawv1.FindClustersRequest) (*lobslawv1.FindClustersResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	q := clusterQuery{
		threshold:       req.Threshold,
		minClusterSize:  int(req.MinClusterSize),
		maxClusterSize:  int(req.MaxClusterSize),
		scopeFilter:     req.ScopeFilter,
		retentionFilter: req.RetentionFilter,
		limit:           int(req.Limit),
	}
	if req.Before != nil {
		q.before = req.Before.AsTime()
	}
	clusters, err := findClusters(s.store, q)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "find clusters: %v", err)
	}
	logging.From(ctx).Debug("find clusters",
		"threshold", q.threshold,
		"retention", q.retentionFilter,
		"count", len(clusters),
	)
	return &lobslawv1.FindClustersResponse{Clusters: clusters}, nil
}

// EpisodicAdd records a single EpisodicRecord through Raft.
func (s *Service) EpisodicAdd(ctx context.Context, req *lobslawv1.EpisodicAddRequest) (*lobslawv1.EpisodicAddResponse, error) {
	if req == nil || req.Record == nil {
		return nil, status.Error(codes.InvalidArgument, "record required")
	}
	// Through the one door. This used to default a few fields and
	// apply the entry itself, which meant every EpisodicAdd caller —
	// research findings, and anything reaching the service over the
	// wire — wrote a record with no owner and no vector: unreadable,
	// because an unowned record is visible only to Everyone(), and
	// findable only by lexical fallback.
	id, err := Remember(ctx, s.raft, s.rememberEmbedder(), 0, req.Record)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	logging.From(ctx).Debug("episodic record added", "id", id, "importance", req.Record.Importance)
	return &lobslawv1.EpisodicAddResponse{Id: id}, nil
}

// rememberEmbedder adapts the service's embedder to what Remember
// needs, or nil when none is configured. Nil is not an error: a node
// with no embeddings still remembers, it just does so without a vector
// index and relies on lexical recall.
func (s *Service) rememberEmbedder() Embedder {
	if s.embedder == nil {
		return nil
	}
	if e, ok := any(s.embedder).(Embedder); ok {
		return e
	}
	return nil
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
//
// Deliberately NOT forwarded, unlike the other write paths. Forget
// scans for a matched set and then deletes it, and the two must agree:
// forwarding each delete individually would run the scan against this
// follower's view while the deletes landed on the leader, so a record
// written in between would be missed by a sweep that reported success.
// It is also an operator action rather than a per-turn write, so
// "retry against the leader" is a reasonable thing to ask of its
// caller — and `lobslaw memory forget` works directly against the
// store while the node is down.
func (s *Service) Forget(ctx context.Context, req *lobslawv1.ForgetRequest) (*lobslawv1.ForgetResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	if req.Query == "" && req.Before == nil && len(req.Tags) == 0 && len(req.Ids) == 0 {
		return nil, status.Error(codes.InvalidArgument,
			"at least one filter (query, before, tags, ids) required — refusing to forget everything")
	}
	if s.raft != nil && !s.raft.IsLeader() {
		return nil, status.Errorf(codes.FailedPrecondition,
			"not the raft leader; retry at %s", s.raft.LeaderAddress())
	}

	var before time.Time
	if req.Before != nil {
		before = req.Before.AsTime()
	}

	// One definition of what a forget matches, shared with the offline
	// path. The scoping happens inside, between matching and cascading,
	// because a record the requester may not read must not pull its
	// consolidations down with it.
	plan, err := PlanForgetFor(s.store, ForgetQuery{
		IDs:    req.Ids,
		Text:   req.Query,
		Before: before,
		Tags:   req.Tags,
	}, forgetAudience(req.Requester))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "forget plan: %v", err)
	}

	if !req.DryRun {
		for _, id := range plan.Matched {
			if derr := s.deleteFromBothBuckets(ctx, id); derr != nil {
				return nil, status.Errorf(codes.Internal, "delete %q: %v", id, derr)
			}
		}
		for _, id := range plan.Swept {
			if derr := s.deleteFromBothBuckets(ctx, id); derr != nil {
				return nil, status.Errorf(codes.Internal, "delete cascade %q: %v", id, derr)
			}
		}
	}

	logging.From(ctx).Info("memory forget",
		"query", req.Query,
		"before", before,
		"tags", req.Tags,
		"direct", len(plan.Matched),
		"cascaded", len(plan.Swept),
		"missing", len(plan.Missing),
		"dry_run", req.DryRun,
	)

	return &lobslawv1.ForgetResponse{
		//nolint:gosec // plan sizes are bounded by the store, not by a caller
		RecordsRemoved: int32(len(plan.Matched)),
		//nolint:gosec // plan sizes are bounded by the store, not by a caller
		ConsolidationsReforged: int32(len(plan.Swept)),
		Matched:                plan.Matched,
		Swept:                  plan.Swept,
		Missing:                plan.Missing,
	}, nil
}

// deleteFromBothBuckets issues a DELETE log entry against both
// VectorRecord and EpisodicRecord buckets. The FSM's applyDelete is
// idempotent for absent keys, so the entry for whichever bucket
// doesn't hold the id is a cheap no-op.
func (s *Service) deleteFromBothBuckets(ctx context.Context, id string) error {
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
		if err := s.applyEntry(ctx, payload); err != nil {
			return err
		}
	}
	return nil
}

// applyEntry proto-marshals e and submits it to Raft. Followers get a
// FailedPrecondition with the leader's address; callers retry there.
func (s *Service) applyEntry(ctx context.Context, e *lobslawv1.LogEntry) error {
	if s.raft == nil {
		return status.Error(codes.Unimplemented, "raft stack not wired on this node")
	}
	data, err := proto.Marshal(e)
	if err != nil {
		return status.Errorf(codes.Internal, "marshal log entry: %v", err)
	}
	resp, err := s.raft.ApplyOrForward(ctx, data, applyTimeout)
	if err != nil {
		return status.Errorf(codes.Internal, "raft apply: %v", err)
	}
	// FSM.Apply can return a plain error; surface it.
	if fsmErr, ok := resp.(error); ok && fsmErr != nil {
		return status.Errorf(codes.Internal, "fsm apply: %v", fsmErr)
	}
	return nil
}

// forgetAudience maps the request's requester onto an Audience. Empty
// is unrestricted: operator tooling and peer nodes reach this RPC over
// mTLS and carry no principal. The agent's memory_forget always sets
// one, so the model never reaches the unrestricted path.
func forgetAudience(requester string) Audience {
	if strings.TrimSpace(requester) == "" {
		return Everyone()
	}
	return For(identity.Principal(requester))
}

// retainForgettable removes ids the audience may not read from the
// matched set, in place.
//
// A record that cannot be found is left in the set: it is either
// already gone or in a bucket this lookup does not cover, and removing
// it here would silently skip a delete the caller asked for.
func retainForgettable(store *Store, matched map[string]struct{}, audience Audience) error {
	for id := range matched {
		vis, found, err := recordVisibility(store, id)
		if err != nil {
			return err
		}
		// Deliberately no session_ref: a conversation-scoped audience
		// widens what a caller may READ, not what they may destroy.
		// Speaking in a channel is not a claim on the records it
		// produced, and Forget is the one operation where being wrong
		// is unrecoverable.
		if found && !audience.allows(vis.owner, vis.visibility, "") {
			delete(matched, id)
		}
	}
	return nil
}

type recordOwnership struct {
	owner      string
	visibility lobslawv1.Visibility
}

// recordVisibility reads a record's ownership from whichever bucket
// holds it. Vector and episodic records share an id space, so both are
// tried — the same reason deleteFromBothBuckets exists.
func recordVisibility(store *Store, id string) (recordOwnership, bool, error) {
	if raw, err := store.Get(BucketVectorRecords, id); err == nil {
		var v lobslawv1.VectorRecord
		if err := proto.Unmarshal(raw, &v); err != nil {
			return recordOwnership{}, false, fmt.Errorf("unmarshal vector %q: %w", id, err)
		}
		return recordOwnership{owner: v.Owner, visibility: v.Visibility}, true, nil
	} else if !IsNotFound(err) {
		return recordOwnership{}, false, err
	}
	if raw, err := store.Get(BucketEpisodicRecords, id); err == nil {
		var e lobslawv1.EpisodicRecord
		if err := proto.Unmarshal(raw, &e); err != nil {
			return recordOwnership{}, false, fmt.Errorf("unmarshal episodic %q: %w", id, err)
		}
		return recordOwnership{owner: e.Owner, visibility: e.Visibility}, true, nil
	} else if !IsNotFound(err) {
		return recordOwnership{}, false, err
	}
	return recordOwnership{}, false, nil
}
