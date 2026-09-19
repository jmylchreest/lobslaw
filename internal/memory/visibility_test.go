package memory

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/identity"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// seedOwned is seedVector with ownership, which the original helper
// predates.
func seedOwned(t *testing.T, s *Store, id string, embedding []float32, owner string, vis lobslawv1.Visibility) {
	t.Helper()
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

func vec(owner string, vis lobslawv1.Visibility) *lobslawv1.VectorRecord {
	return &lobslawv1.VectorRecord{Owner: owner, Visibility: vis}
}

func TestAudienceAllows(t *testing.T) {
	t.Parallel()
	alice := identity.User("alice")
	bob := identity.User("bob")

	cases := []struct {
		name     string
		audience Audience
		record   *lobslawv1.VectorRecord
		want     bool
	}{
		// The whole point of the type: an Audience nobody set matches
		// nothing, so forgetting to pass one yields empty results
		// rather than everyone's memories.
		{"zero audience sees nothing", Audience{},
			vec("user:alice", lobslawv1.Visibility_VISIBILITY_PRIVATE), false},
		{"zero audience sees nothing, even legacy", Audience{},
			vec("", lobslawv1.Visibility_VISIBILITY_UNSPECIFIED), false},

		{"owner sees own", For(alice),
			vec("user:alice", lobslawv1.Visibility_VISIBILITY_PRIVATE), true},
		{"other does not see private", For(bob),
			vec("user:alice", lobslawv1.Visibility_VISIBILITY_PRIVATE), false},
		{"other sees shared", For(bob),
			vec("user:alice", lobslawv1.Visibility_VISIBILITY_SHARED), true},

		// An unowned record is readable by nobody but Everyone(). The
		// carve-out that made it readable by all was for records
		// predating ownership, and lobslaw has never been deployed, so
		// that population is empty — it was a fail-open guarding
		// nothing. An unowned record now means a bug upstream.
		{"unowned readable by nobody", For(bob),
			vec("", lobslawv1.Visibility_VISIBILITY_UNSPECIFIED), false},

		// Anonymous owns nothing but is not blind: it still sees the
		// records nobody owns and the ones marked shared.
		{"anonymous sees no unowned", For(""),
			vec("", lobslawv1.Visibility_VISIBILITY_UNSPECIFIED), false},
		{"anonymous sees shared", For(""),
			vec("user:alice", lobslawv1.Visibility_VISIBILITY_SHARED), true},
		{"anonymous sees no private", For(""),
			vec("user:alice", lobslawv1.Visibility_VISIBILITY_PRIVATE), false},

		// An owned record with UNSPECIFIED visibility is not legacy —
		// something set an owner. Treat it as private, or a writer that
		// forgets the visibility field silently publishes.
		{"owned but unspecified is not public", For(bob),
			vec("user:alice", lobslawv1.Visibility_VISIBILITY_UNSPECIFIED), false},

		{"everyone sees private", Everyone(),
			vec("user:alice", lobslawv1.Visibility_VISIBILITY_PRIVATE), true},

		// A bot reads its own diary and the memory of the human it
		// serves, and nothing else.
		{"bot sees own", ForWith(identity.Bot("designer"), alice),
			vec("bot:designer", lobslawv1.Visibility_VISIBILITY_PRIVATE), true},
		{"bot sees its owner", ForWith(identity.Bot("designer"), alice),
			vec("user:alice", lobslawv1.Visibility_VISIBILITY_PRIVATE), true},
		{"bot does not see another person", ForWith(identity.Bot("designer"), alice),
			vec("user:bob", lobslawv1.Visibility_VISIBILITY_PRIVATE), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.audience.AllowsVector(tc.record); got != tc.want {
				t.Errorf("allows = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestVectorSearchRefusesUnsetAudience is the property the type exists
// for. A caller that forgets the audience gets nothing, not everything.
func TestVectorSearchRefusesUnsetAudience(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	seedOwned(t, s, "v1", []float32{1, 0, 0}, "user:alice", lobslawv1.Visibility_VISIBILITY_PRIVATE)

	hits, err := vectorSearch(s, []float32{1, 0, 0}, 10, Audience{}, "", lobslawv1.Retention_RETENTION_UNSPECIFIED)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("an unset audience returned %d records; the zero value must match nothing", len(hits))
	}
}

func TestVectorSearchScopesToOwner(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	seedOwned(t, s, "a", []float32{1, 0, 0}, "user:alice", lobslawv1.Visibility_VISIBILITY_PRIVATE)
	seedOwned(t, s, "b", []float32{1, 0, 0}, "user:bob", lobslawv1.Visibility_VISIBILITY_PRIVATE)
	seedOwned(t, s, "legacy", []float32{1, 0, 0}, "", lobslawv1.Visibility_VISIBILITY_UNSPECIFIED)

	hits, err := vectorSearch(s, []float32{1, 0, 0}, 10, For(identity.User("alice")), "", lobslawv1.Retention_RETENTION_UNSPECIFIED)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, h := range hits {
		got[h.Record().Id] = true
	}
	if !got["a"] {
		t.Errorf("alice lost her own record: %v", got)
	}
	if got["legacy"] {
		t.Error("an unowned record was returned; unowned is readable by nobody")
	}
	if got["b"] {
		t.Error("alice was handed bob's private record")
	}
}

// TestNoUnscopedVectorSearch guards the invariant rather than the
// instances. The original leak was two call sites passing "" to a
// string filter; enumerating today's callers cannot catch tomorrow's,
// so this reads the tree and fails on the shape.
func TestNoUnscopedVectorSearch(t *testing.T) {
	t.Parallel()
	// Walk from the repo root so this covers every package, not just
	// this one — the leaks were in internal/compute.
	root := "../.."
	fset := token.NewFileSet()

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			// Vendored, generated and agent-worktree trees are not ours.
			switch info.Name() {
			case ".git", ".claude", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				name = fn.Name
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			}
			if name != "VectorSearch" && name != "vectorSearch" {
				return true
			}
			// audience is the 4th argument. A string literal there is
			// the old signature — either a stale call or someone
			// reaching for the scope filter by position.
			if len(call.Args) < 4 {
				return true
			}
			if lit, ok := call.Args[3].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				t.Errorf("%s: vector search called with a string where the Audience goes.\n"+
					"    Reads must be scoped: pass memory.For(principal), or\n"+
					"    memory.Everyone() if this caller genuinely holds the whole store.",
					fset.Position(call.Pos()))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
