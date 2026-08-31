package compute

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// The counterpart of a disputed memory is re-checked against the
// audience, not trusted because it shares a verdict with something
// readable.
//
// Clustering never crosses owners, so a cross-owner verdict should be
// impossible — which is exactly why this is worth a test. The check
// is here for the case where that invariant is broken by a legacy
// record, a repaired store, or a future writer, and rendering the
// counterpart would leak it through the back door of an argument
// about it.
func TestDisputedCounterpartIsAudienceChecked(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)

	mine := &lobslawv1.EpisodicRecord{
		Id: "mine", Owner: "user:alice", Context: "alice is vegetarian",
		Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE,
		Timestamp:  timestamppb.New(time.Now()),
	}
	theirs := &lobslawv1.EpisodicRecord{
		Id: "theirs", Owner: "user:bob", Context: "bob's private confession",
		Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE,
		Timestamp:  timestamppb.New(time.Now()),
	}
	putEpisodic(t, store, mine)
	putEpisodic(t, store, theirs)
	putDispute(t, store, &lobslawv1.ConsolidationRecord{
		Id: "c1", ClusterId: "cl1", Verdict: string(memory.VerdictConflict),
		Reason: "which is it?", SourceIds: []string{"mine", "theirs"},
		Owner: "user:alice", CreatedAt: timestamppb.New(time.Now()),
	})

	e := NewContextEngine(ContextEngineConfig{Store: store})
	entries := []recallEntry{{rec: mine, score: 0.9}}
	e.annotateDisputes(memory.For(identity.User("alice")), entries)

	if entries[0].dispute != nil {
		t.Fatalf("rendered another owner's record as the other side: %q",
			entries[0].dispute.counterpart)
	}

	got := e.assemble(entries, "test")
	if len(got.Blocks) != 1 || strings.Contains(got.Blocks[0].Content, "confession") {
		t.Errorf("bob's private record reached alice's prompt: %+v", got.Blocks)
	}
}

func putEpisodic(t *testing.T, s *memory.Store, rec *lobslawv1.EpisodicRecord) {
	t.Helper()
	raw, err := proto.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(memory.BucketEpisodicRecords, rec.Id, raw); err != nil {
		t.Fatal(err)
	}
}

func putDispute(t *testing.T, s *memory.Store, rec *lobslawv1.ConsolidationRecord) {
	t.Helper()
	raw, err := proto.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(memory.BucketConsolidations, rec.Id, raw); err != nil {
		t.Fatal(err)
	}
	for _, sid := range rec.SourceIds {
		if err := s.Put(memory.BucketDisputes, sid+"/"+rec.Id, []byte(rec.Id)); err != nil {
			t.Fatal(err)
		}
	}
}
