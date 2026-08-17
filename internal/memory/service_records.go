package memory

import (
	"context"

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
