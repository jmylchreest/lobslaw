package memory

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Browsing memory from an operator's laptop.
//
// `lobslaw memory list` and `memory show` opened state.db directly,
// which needs the node STOPPED — and on a laptop there is no state.db
// to open at all. An empty listing then reads as an empty cluster,
// which is the confidently-wrong answer R28 exists to remove.
//
// Both delegate to QueryRecords / FindRecord, the same functions the
// offline path uses. A second scan here would mean two definitions of
// what "--unowned" selects, and they would drift the day somebody
// fixed one of them.

// ListRecords browses the record buckets.
func (s *Service) ListRecords(_ context.Context, req *lobslawv1.ListRecordsRequest) (
	*lobslawv1.ListRecordsResponse, error) {
	if s.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node has no memory store")
	}
	if req == nil {
		req = &lobslawv1.ListRecordsRequest{}
	}

	filter := RecordFilter{
		Kind:    req.GetKind(),
		Owner:   req.GetOwner(),
		Scope:   req.GetScope(),
		Tag:     req.GetTag(),
		Unowned: req.GetUnowned(),
		Limit:   int(req.GetLimit()),
	}
	if err := filter.Validate(); err != nil {
		// The caller's mistake, not the server's: a mistyped kind that
		// silently meant "all" would show records they asked to exclude.
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	page, err := QueryRecords(s.store, filter)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list records: %v", err)
	}

	return &lobslawv1.ListRecordsResponse{
		Vectors:   page.Vectors,
		Episodics: page.Episodics,
		//nolint:gosec // bucket counts are not attacker-controlled
		VectorTotal: int32(page.VectorTotal),
		//nolint:gosec // bucket counts are not attacker-controlled
		EpisodicTotal: int32(page.EpisodicTotal),
		//nolint:gosec // bucket counts are not attacker-controlled
		Unowned: int32(page.Unowned),
	}, nil
}

// GetRecord returns one record and what a forget would sweep with it.
func (s *Service) GetRecord(_ context.Context, req *lobslawv1.GetRecordRequest) (
	*lobslawv1.GetRecordResponse, error) {
	if s.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node has no memory store")
	}
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}

	vec, epi, err := FindRecord(s.store, req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get record: %v", err)
	}
	if vec == nil && epi == nil {
		// NOT_FOUND rather than an empty success. An empty record and a
		// record that is not there are different answers, and only one
		// of them means the id was wrong.
		return nil, status.Errorf(codes.NotFound, "no vector or episodic record with id %q", req.GetId())
	}

	refs, err := ReferencedBy(s.store, req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "referenced by: %v", err)
	}

	return &lobslawv1.GetRecordResponse{
		Vector:       vec,
		Episodic:     epi,
		ReferencedBy: refs,
	}, nil
}

// ListConsolidations reads Dream's adjudication log.
//
// The offline `lobslaw memory consolidations` cannot run while the
// node is up: bbolt holds an exclusive flock on state.db for the life
// of the process, so the CLI's own open times out. An audit trail
// readable only by stopping the thing it audits is one nobody reads,
// and this is the whole justification for letting Dream rewrite
// memory at all.
func (s *Service) ListConsolidations(_ context.Context, req *lobslawv1.ListConsolidationsRequest) (
	*lobslawv1.ListConsolidationsResponse, error) {
	if s.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node has no memory store")
	}
	if req == nil {
		req = &lobslawv1.ListConsolidationsRequest{}
	}
	q := ConsolidationQuery{
		Owner:   req.GetOwner(),
		Verdict: req.GetVerdict(),
		Limit:   int(req.GetLimit()),
	}
	if secs := req.GetSinceSeconds(); secs > 0 {
		q.Since = time.Now().Add(-time.Duration(secs) * time.Second)
	}
	entries, err := ListConsolidations(s.store, q)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list consolidations: %v", err)
	}
	return &lobslawv1.ListConsolidationsResponse{Consolidations: entries}, nil
}

// SetRecordVisibility shares or unshares records.
//
// Planned and applied on the server, even for a dry run. A client
// cannot plan this while the node is up — bbolt's lock is exclusive —
// and a preview computed against a different view of the store than
// the write is a preview of nothing.
//
// Refuses an unowned record rather than sharing it. An owner-less
// record is a leftover or a write path that skipped the field; either
// way it belongs to nobody, and "shared with everyone" is the one
// change that cannot be walked back by finding out whose it was.
func (s *Service) SetRecordVisibility(ctx context.Context, req *lobslawv1.SetRecordVisibilityRequest) (
	*lobslawv1.SetRecordVisibilityResponse, error) {
	if s.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node has no memory store")
	}
	if req == nil || len(req.GetIds()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one record id required")
	}
	to := req.GetVisibility()
	if to == lobslawv1.Visibility_VISIBILITY_UNSPECIFIED {
		return nil, status.Error(codes.InvalidArgument, "visibility required")
	}
	if req.GetApply() && s.raft != nil && !s.raft.IsLeader() {
		return nil, status.Errorf(codes.FailedPrecondition,
			"not the raft leader; retry at %s", s.raft.LeaderAddress())
	}

	plan, err := PlanVisibility(s.store, req.GetIds(), to)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	out := &lobslawv1.SetRecordVisibilityResponse{
		Changes: make([]*lobslawv1.VisibilityChange, 0, len(plan)),
		Applied: req.GetApply(),
	}
	for _, c := range plan {
		out.Changes = append(out.Changes, &lobslawv1.VisibilityChange{
			Id: c.ID, Kind: c.Kind, Owner: c.Owner,
			From: c.From, To: c.To, Changed: c.From != c.To,
		})
		if !req.GetApply() || c.From == c.To {
			continue
		}
		if err := s.applyEntry(ctx, c.entry()); err != nil {
			return nil, status.Errorf(codes.Internal, "write %s: %v", c.ID, err)
		}
	}
	return out, nil
}
