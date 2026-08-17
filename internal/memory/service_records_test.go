package memory

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// `memory list` and `memory show` could only read a state.db on the
// same filesystem, so on an operator's laptop they listed nothing —
// and an empty listing reads as an empty cluster.

func putVector(t *testing.T, s *Service, id, owner, scope string) {
	t.Helper()
	_, err := s.Store(context.Background(), &lobslawv1.StoreRequest{
		Record: &lobslawv1.VectorRecord{
			Id: id, Owner: owner, Scope: scope, Text: "text for " + id,
			Embedding: []float32{0.1, 0.2}, CreatedAt: timestamppb.Now(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func putEpisodic(t *testing.T, s *Service, id, owner string, tags ...string) {
	t.Helper()
	_, err := s.EpisodicAdd(context.Background(), &lobslawv1.EpisodicAddRequest{
		Record: &lobslawv1.EpisodicRecord{
			Id: id, Owner: owner, Event: "event " + id, Tags: tags,
			Timestamp: timestamppb.Now(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func list(t *testing.T, s *Service, req *lobslawv1.ListRecordsRequest) *lobslawv1.ListRecordsResponse {
	t.Helper()
	res, err := s.ListRecords(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// --- listing -----------------------------------------------------------

func TestListReturnsBothKinds(t *testing.T) {
	t.Parallel()
	s := newTestServiceStack(t)
	putVector(t, s, "v1", "user:alice", "work")
	putEpisodic(t, s, "e1", "user:alice", "meeting")

	res := list(t, s, &lobslawv1.ListRecordsRequest{})
	if len(res.GetVectors()) != 1 || len(res.GetEpisodics()) != 1 {
		t.Fatalf("vectors=%d episodics=%d", len(res.GetVectors()), len(res.GetEpisodics()))
	}
}

func TestKindNarrowsToOneBucket(t *testing.T) {
	t.Parallel()
	s := newTestServiceStack(t)
	putVector(t, s, "v1", "user:alice", "work")
	putEpisodic(t, s, "e1", "user:alice")

	if res := list(t, s, &lobslawv1.ListRecordsRequest{Kind: "vector"}); len(res.GetEpisodics()) != 0 {
		t.Errorf("--kind vector returned %d episodic records", len(res.GetEpisodics()))
	}
	if res := list(t, s, &lobslawv1.ListRecordsRequest{Kind: "episodic"}); len(res.GetVectors()) != 0 {
		t.Errorf("--kind episodic returned %d vector records", len(res.GetVectors()))
	}
}

// A mistyped kind that silently meant "all" would show records the
// caller asked to exclude.
func TestAnUnknownKindIsRefused(t *testing.T) {
	t.Parallel()
	s := newTestServiceStack(t)

	_, err := s.ListRecords(context.Background(), &lobslawv1.ListRecordsRequest{Kind: "vectors"})
	if err == nil {
		t.Fatal("an unknown kind was accepted")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", status.Code(err))
	}
}

// Scope exists only on vector records. Returning episodics unfiltered
// would read as "these episodics carry that scope".
func TestScopeExcludesEpisodicsOutright(t *testing.T) {
	t.Parallel()
	s := newTestServiceStack(t)
	putVector(t, s, "v1", "user:alice", "work")
	putVector(t, s, "v2", "user:alice", "home")
	putEpisodic(t, s, "e1", "user:alice")

	res := list(t, s, &lobslawv1.ListRecordsRequest{Scope: "work"})
	if len(res.GetEpisodics()) != 0 {
		t.Errorf("a scope filter returned %d episodic records", len(res.GetEpisodics()))
	}
	if len(res.GetVectors()) != 1 || res.GetVectors()[0].GetId() != "v1" {
		t.Errorf("vectors = %v", res.GetVectors())
	}
}

// Tags exist only on episodic records, for the same reason.
func TestTagExcludesVectorsOutright(t *testing.T) {
	t.Parallel()
	s := newTestServiceStack(t)
	putVector(t, s, "v1", "user:alice", "work")
	putEpisodic(t, s, "e1", "user:alice", "meeting")
	putEpisodic(t, s, "e2", "user:alice", "other")

	res := list(t, s, &lobslawv1.ListRecordsRequest{Tag: "meeting"})
	if len(res.GetVectors()) != 0 {
		t.Errorf("a tag filter returned %d vector records", len(res.GetVectors()))
	}
	if len(res.GetEpisodics()) != 1 || res.GetEpisodics()[0].GetId() != "e1" {
		t.Errorf("episodics = %v", res.GetEpisodics())
	}
}

// --unowned and an empty --owner are different questions. Folding them
// into one would make the second unreachable.
func TestUnownedIsNotAnEmptyOwnerFilter(t *testing.T) {
	t.Parallel()
	s := newTestServiceStack(t)
	putVector(t, s, "v1", "user:alice", "")
	putVector(t, s, "v2", "", "")

	all := list(t, s, &lobslawv1.ListRecordsRequest{Owner: ""})
	if len(all.GetVectors()) != 2 {
		t.Errorf("an empty owner filter returned %d records; it should not filter", len(all.GetVectors()))
	}

	orphans := list(t, s, &lobslawv1.ListRecordsRequest{Unowned: true})
	if len(orphans.GetVectors()) != 1 || orphans.GetVectors()[0].GetId() != "v2" {
		t.Errorf("--unowned = %v", orphans.GetVectors())
	}
	if orphans.GetUnowned() != 1 {
		t.Errorf("unowned count = %d", orphans.GetUnowned())
	}
}

// The totals are the counts BEFORE the limit, so a truncated listing
// can say what it is not showing rather than implying it is all there
// is.
func TestTheTotalsSurviveTheLimit(t *testing.T) {
	t.Parallel()
	s := newTestServiceStack(t)
	for _, id := range []string{"v1", "v2", "v3"} {
		putVector(t, s, id, "user:alice", "")
	}

	res := list(t, s, &lobslawv1.ListRecordsRequest{Limit: 1})
	if len(res.GetVectors()) != 1 {
		t.Fatalf("limit returned %d records", len(res.GetVectors()))
	}
	if res.GetVectorTotal() != 3 {
		t.Errorf("vector_total = %d; a truncated listing reads as the whole store", res.GetVectorTotal())
	}
}

// --- one record --------------------------------------------------------

func TestGetRecordFindsEitherKind(t *testing.T) {
	t.Parallel()
	s := newTestServiceStack(t)
	putVector(t, s, "v1", "user:alice", "work")
	putEpisodic(t, s, "e1", "user:alice")

	vec, err := s.GetRecord(context.Background(), &lobslawv1.GetRecordRequest{Id: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if vec.GetVector() == nil || vec.GetEpisodic() != nil {
		t.Errorf("v1 came back as %+v", vec)
	}

	epi, err := s.GetRecord(context.Background(), &lobslawv1.GetRecordRequest{Id: "e1"})
	if err != nil {
		t.Fatal(err)
	}
	if epi.GetEpisodic() == nil || epi.GetVector() != nil {
		t.Errorf("e1 came back as %+v", epi)
	}
}

// An empty record and a record that is not there are different
// answers, and only one of them means the id was wrong.
func TestAMissingRecordIsNotFoundNotAnEmptySuccess(t *testing.T) {
	t.Parallel()
	s := newTestServiceStack(t)

	_, err := s.GetRecord(context.Background(), &lobslawv1.GetRecordRequest{Id: "ghost"})
	if err == nil {
		t.Fatal("a missing record returned a success")
	}
	if status.Code(err) != codes.NotFound {
		t.Errorf("code = %v, want NotFound", status.Code(err))
	}
}

func TestGetRecordNeedsAnId(t *testing.T) {
	t.Parallel()
	s := newTestServiceStack(t)
	if _, err := s.GetRecord(context.Background(), &lobslawv1.GetRecordRequest{}); err == nil {
		t.Error("an empty id was accepted")
	}
}

// What a forget would take with it, returned beside the record —
// finding that out afterwards is too late.
func TestGetRecordReportsWhatAForgetWouldSweep(t *testing.T) {
	t.Parallel()
	s := newTestServiceStack(t)
	putVector(t, s, "v1", "user:alice", "")
	_, err := s.Store(context.Background(), &lobslawv1.StoreRequest{
		Record: &lobslawv1.VectorRecord{
			Id: "summary", Owner: "user:alice", Text: "consolidated",
			SourceIds: []string{"v1"}, Embedding: []float32{0.3},
			CreatedAt: timestamppb.Now(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.GetRecord(context.Background(), &lobslawv1.GetRecordRequest{Id: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetReferencedBy()) != 1 || res.GetReferencedBy()[0] != "summary" {
		t.Errorf("referenced_by = %v", res.GetReferencedBy())
	}
}
