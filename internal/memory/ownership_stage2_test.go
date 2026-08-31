package memory

import (
	"testing"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Dream consolidation replaces a cluster's members with one summary
// carrying all their SourceIds. Clustering across owners would mint a
// record holding two people's memories and owned by neither — and
// unowned reads as legacy, so everyone would see it. No read-side
// filter can undo that after the merge.
func TestFindClustersNeverGroupsAcrossOwners(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)

	// Identical embeddings: without the owner check these are the
	// most obvious near-duplicate pair possible.
	seedOwnedVector(t, s, "alice-1", []float32{1, 0, 0}, "user:alice")
	seedOwnedVector(t, s, "bob-1", []float32{1, 0, 0}, "user:bob")

	clusters, err := findClusters(s, clusterQuery{threshold: 0.9, minClusterSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range clusters {
		owners := map[string]bool{}
		for _, r := range c.Records {
			owners[r.Owner] = true
		}
		if len(owners) > 1 {
			t.Errorf("cluster %s spans owners %v — consolidating it would mint a record owned by neither", c.Id, owners)
		}
	}
}

// Two records with the same owner are still eligible: the guard must
// scope clustering, not disable it.
func TestFindClustersStillGroupsWithinAnOwner(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	seedOwnedVector(t, s, "alice-1", []float32{1, 0, 0}, "user:alice")
	seedOwnedVector(t, s, "alice-2", []float32{1, 0, 0}, "user:alice")

	clusters, err := findClusters(s, clusterQuery{threshold: 0.9, minClusterSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 || len(clusters[0].Records) != 2 {
		t.Fatalf("expected one 2-member cluster, got %d clusters", len(clusters))
	}
}

// Forget is destructive and cascades through consolidations, so an
// unscoped one lets any caller erase someone else's memory.
func TestForgetRefusesAnotherPrincipalsRecord(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	seedOwnedVector(t, s, "alice-secret", []float32{1, 0, 0}, "user:alice")

	matched := map[string]struct{}{"alice-secret": {}}
	if err := retainForgettable(s, matched, For("user:bob")); err != nil {
		t.Fatal(err)
	}
	if _, still := matched["alice-secret"]; still {
		t.Error("bob was allowed to forget alice's record")
	}
}

func TestForgetAllowsOwnRecords(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	seedOwnedVector(t, s, "alice-own", []float32{1, 0, 0}, "user:alice")
	// Unowned is readable by nobody, so it is forgettable by nobody
	// either — the operator path (empty requester) still reaches it.
	seedOwnedVector(t, s, "unowned", []float32{1, 0, 0}, "")

	matched := map[string]struct{}{"alice-own": {}, "unowned": {}}
	if err := retainForgettable(s, matched, For("user:alice")); err != nil {
		t.Fatal(err)
	}
	if _, ok := matched["alice-own"]; !ok {
		t.Error("alice lost the right to forget her own record")
	}
	if _, ok := matched["unowned"]; ok {
		t.Error("a named principal forgot an unowned record")
	}

	operator := map[string]struct{}{"unowned": {}}
	if err := retainForgettable(s, operator, forgetAudience("")); err != nil {
		t.Fatal(err)
	}
	if len(operator) != 1 {
		t.Error("the operator path cannot reach an unowned record; nothing could clean it up")
	}
}

// An empty requester is the operator path, and must stay unrestricted
// or `lobslaw` tooling cannot clean up after anyone.
func TestForgetAudienceEmptyRequesterIsUnrestricted(t *testing.T) {
	t.Parallel()
	if !forgetAudience("").allows("user:alice", lobslawv1.Visibility_VISIBILITY_PRIVATE, "") {
		t.Error("an empty requester should be the unrestricted operator path")
	}
	if forgetAudience("user:bob").allows("user:alice", lobslawv1.Visibility_VISIBILITY_PRIVATE, "") {
		t.Error("a named requester must not reach another principal's private record")
	}
}

func seedOwnedVector(t *testing.T, s *Store, id string, embedding []float32, owner string) {
	t.Helper()
	vis := lobslawv1.Visibility_VISIBILITY_UNSPECIFIED
	if owner != "" {
		vis = lobslawv1.Visibility_VISIBILITY_PRIVATE
	}
	raw, err := proto.Marshal(&lobslawv1.VectorRecord{
		Id: id, Embedding: embedding, Owner: owner, Visibility: vis,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(BucketVectorRecords, id, raw); err != nil {
		t.Fatal(err)
	}
}
