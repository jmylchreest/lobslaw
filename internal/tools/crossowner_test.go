package tools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// stubAuthorizer stands in for the node's policy-backed authorizer.
// grant is the answer a rule would have produced; seen records the
// claims it was asked about, which is how the identity tests assert
// that roles survived the trip from the channel to here.
type stubAuthorizer struct {
	grant bool
	seen  *types.Claims
	calls int
}

func (s *stubAuthorizer) AllowsAny(_ context.Context, claims *types.Claims) bool {
	s.calls++
	s.seen = claims
	return s.grant
}

// fixedEmbedder returns the same vector for everything, so cosine
// similarity is 1.0 for every stored record and the ownership filter
// is the only thing that can remove a result.
type fixedEmbedder struct{}

func (fixedEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{1, 0}, nil
}

func (fixedEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func (fixedEmbedder) Dimensions() int { return 2 }

func seedVector(t *testing.T, store *memory.Store, rec *lobslawv1.VectorRecord) {
	t.Helper()
	raw, err := proto.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(memory.BucketVectorRecords, rec.Id, raw); err != nil {
		t.Fatal(err)
	}
}

// seedTwoOwners writes one record for alice and one for bob, each
// with a vector row pointing at it. The two texts share no tokens so
// the substring augmentation in memory_search cannot leak one into
// the other's result set by accident.
func seedTwoOwners(t *testing.T, store *memory.Store) {
	t.Helper()
	for _, r := range []struct {
		id, owner, event string
	}{
		{"alice-rec", "user:alice", "alice recorded her sourdough starter schedule"},
		{"bob-rec", "user:bob", "bob logged a cardiology appointment"},
	} {
		seedEpisodic(t, store, &lobslawv1.EpisodicRecord{
			Id: r.id, Event: r.event, Context: r.event,
			Importance: 5, Timestamp: timestamppb.Now(),
			Owner: r.owner, Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE,
		})
		seedVector(t, store, &lobslawv1.VectorRecord{
			Id: "vec-" + r.id, Embedding: []float32{1, 0},
			SourceIds: []string{r.id},
			Owner:     r.owner, Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE,
		})
	}
}

// operatorTurn is alice arriving with role:operator declared. The
// role alone is what the naive implementation would have keyed on.
func operatorTurn(ctx context.Context) context.Context {
	return turn.WithIdentity(ctx, turn.Identity{
		UserID:    "alice",
		Principal: identity.User("alice"),
		Scope:     "default",
		Roles:     []string{"operator"},
	})
}

func searchIDs(t *testing.T, out []byte) []string {
	t.Helper()
	var payload struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(payload.Results))
	for _, r := range payload.Results {
		id, _ := r["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func contains(ids []string, want string) bool {
	return slices.Contains(ids, want)
}

// TestMemorySearchWideningIsPolicyDrivenNotRoleDriven is the pair the
// decision turns on: identical claims, identical records, and the
// only difference is what the authorizer answers. If the role were
// the grant, both halves would return bob's record.
func TestMemorySearchWideningIsPolicyDrivenNotRoleDriven(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		grant   bool
		wantBob bool
	}{
		{name: "policy grants the widening", grant: true, wantBob: true},
		{name: "role held but policy silent", grant: false, wantBob: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newMemoryStoreForTest(t)
			seedTwoOwners(t, store)

			authz := &stubAuthorizer{grant: tc.grant}
			b := NewBuiltins()
			if err := RegisterMemoryBuiltins(b, MemoryConfig{
				Store: store, Raft: &fakeApplier{},
				Embedder: fixedEmbedder{}, CrossOwner: authz,
			}); err != nil {
				t.Fatal(err)
			}
			fn, _ := b.Get("memory_search")
			out, _, err := fn(operatorTurn(context.Background()),
				map[string]string{"query": "starter"})
			if err != nil {
				t.Fatal(err)
			}
			ids := searchIDs(t, out)
			if !contains(ids, "alice-rec") {
				t.Errorf("caller should always see their own record; got %v", ids)
			}
			if got := contains(ids, "bob-rec"); got != tc.wantBob {
				t.Errorf("sees bob's record = %v; want %v (ids %v)", got, tc.wantBob, ids)
			}
			if authz.calls == 0 || authz.seen == nil {
				t.Fatal("memory_search never asked the authorizer")
			}
			if !authz.seen.HasRole("operator") {
				t.Errorf("authorizer saw roles %v; want the turn's operator role", authz.seen.Roles)
			}
		})
	}
}

// TestMemorySearchNilAuthorizerDoesNotWiden pins the default. A
// deployment that never wired an authorizer has not said operators
// may read everything, and must not be read as having said it.
func TestMemorySearchNilAuthorizerDoesNotWiden(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	seedTwoOwners(t, store)

	b := NewBuiltins()
	if err := RegisterMemoryBuiltins(b, MemoryConfig{
		Store: store, Raft: &fakeApplier{}, Embedder: fixedEmbedder{},
	}); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("memory_search")
	out, _, err := fn(operatorTurn(context.Background()),
		map[string]string{"query": "starter"})
	if err != nil {
		t.Fatal(err)
	}
	if ids := searchIDs(t, out); contains(ids, "bob-rec") {
		t.Errorf("nil authorizer widened the read; got %v", ids)
	}
}

// TestMemorySearchNonOperatorUnaffected covers the ordinary turn: no
// roles, and the authorizer answering no, which is what the real one
// does for a subject no rule matches.
func TestMemorySearchNonOperatorUnaffected(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	seedTwoOwners(t, store)

	b := NewBuiltins()
	if err := RegisterMemoryBuiltins(b, MemoryConfig{
		Store: store, Raft: &fakeApplier{}, Embedder: fixedEmbedder{},
		CrossOwner: &stubAuthorizer{grant: false},
	}); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("memory_search")
	ctx := turn.WithIdentity(context.Background(), turn.Identity{
		UserID: "alice", Principal: identity.User("alice"), Scope: "default",
	})
	out, _, err := fn(ctx, map[string]string{"query": "starter"})
	if err != nil {
		t.Fatal(err)
	}
	ids := searchIDs(t, out)
	if !contains(ids, "alice-rec") || contains(ids, "bob-rec") {
		t.Errorf("non-operator should see only their own record; got %v", ids)
	}
}

// TestContextEngineWideningFollowsPolicy covers passive recall, which
// is the path with no tool call in front of it — the one where a
// silent widening would put another person's memories into the system
// prompt before the user has said anything.
func TestContextEngineWideningFollowsPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		authz   compute.CrossOwnerAuthorizer
		wantBob bool
	}{
		{name: "policy grants the widening", authz: &stubAuthorizer{grant: true}, wantBob: true},
		{name: "policy silent", authz: &stubAuthorizer{grant: false}, wantBob: false},
		{name: "no authorizer wired", authz: nil, wantBob: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newMemoryStoreForTest(t)
			seedTwoOwners(t, store)

			e := compute.NewContextEngine(compute.ContextEngineConfig{
				Store: store, Embedder: fixedEmbedder{}, CrossOwner: tc.authz,
			})
			got := e.Assemble(operatorTurn(context.Background()), "what is on my plate").Rendered()
			if !strings.Contains(got, "sourdough") {
				t.Errorf("caller's own record missing from recall: %q", got)
			}
			leaked := strings.Contains(got, "cardiology")
			if leaked != tc.wantBob {
				t.Errorf("bob's record in prompt = %v; want %v", leaked, tc.wantBob)
			}
		})
	}
}

// TestMemoryForgetWidensForAPolicyGrantedPrincipal covers the
// destructive path: an operator cleaning up after someone who has
// left needs to reach records they do not own, and the alternative
// to authorising it here is that they do it from an unauthenticated
// CLI where nothing records who did it.
func TestMemoryForgetWidensForAPolicyGrantedPrincipal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		authz         compute.CrossOwnerAuthorizer
		wantRequester string
	}{
		{name: "granted", authz: &stubAuthorizer{grant: true}, wantRequester: ""},
		{name: "not granted", authz: &stubAuthorizer{grant: false}, wantRequester: "user:alice"},
		{name: "no authorizer wired", authz: nil, wantRequester: "user:alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &recordingForgetter{}
			b := NewBuiltins()
			if err := RegisterMemoryBuiltins(b, MemoryConfig{
				Store: newMemoryStoreForTest(t), Raft: &fakeApplier{},
				Forgetter: f, CrossOwner: tc.authz,
			}); err != nil {
				t.Fatal(err)
			}
			fn, _ := b.Get("memory_forget")
			if _, _, err := fn(operatorTurn(context.Background()),
				map[string]string{"query": "cardiology"}); err != nil {
				t.Fatal(err)
			}
			if f.last == nil {
				t.Fatal("forget never reached the service")
			}
			if f.last.Requester != tc.wantRequester {
				t.Errorf("requester = %q; want %q", f.last.Requester, tc.wantRequester)
			}
		})
	}
}

type recordingForgetter struct {
	last *lobslawv1.ForgetRequest
}

func (r *recordingForgetter) Forget(_ context.Context, req *lobslawv1.ForgetRequest) (*lobslawv1.ForgetResponse, error) {
	r.last = req
	return &lobslawv1.ForgetResponse{}, nil
}

func (fixedEmbedder) Model() string { return "test-embedder-v1" }

func (f fixedEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return f.Embed(ctx, text)
}
