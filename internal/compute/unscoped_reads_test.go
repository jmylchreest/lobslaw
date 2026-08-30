package compute

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// memoryBuckets are the stores holding user-owned records. A read of
// either has to decide who may see the result.
var memoryBuckets = map[string]bool{
	"BucketEpisodicRecords": true,
	"BucketVectorRecords":   true,
}

// audienceMarkers are the ways a function can demonstrate it consulted
// visibility. Named rather than inferred: a dataflow analysis would be
// more precise, and would also be a program nobody maintains.
var audienceMarkers = map[string]bool{
	"AllowsEpisodic": true,
	"AllowsVector":   true,
	"ReadAudience":   true,
	"For":            true,
	"Everyone":       true,
}

// TestNoUnscopedMemoryBucketReads is the third iteration of this guard,
// and the history is the argument for its current shape.
//
// v1 asserted that every call to VectorSearch passed an Audience. It
// passed for a week while memory_search leaked every owner's records
// through RunSubstringSearch, which walks the episodic bucket directly
// and never calls VectorSearch at all.
//
// v2 widened to the bucket but checked per FILE — does a file that
// reads a memory bucket mention an audience anywhere. It passed on a
// branch that still contained an unscoped dream_recap, because other
// functions in the same file had been fixed and the word appeared
// nearby.
//
// So: per FUNCTION. A function that reads a memory bucket must itself
// consult visibility, or name a helper that does. Both earlier versions
// were narrower than the property they claimed to protect, which is
// worth remembering before trusting the next structural test — this one
// included.
func TestNoUnscopedMemoryBucketReads(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()

	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var readsBucket, consultsAudience bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch e := n.(type) {
				case *ast.SelectorExpr:
					if memoryBuckets[e.Sel.Name] {
						readsBucket = true
					}
					if audienceMarkers[e.Sel.Name] {
						consultsAudience = true
					}
				case *ast.Ident:
					if audienceMarkers[e.Name] {
						consultsAudience = true
					}
				}
				return true
			})
			// A closure inside the function counts as part of it: the
			// bucket scans here are all ForEach callbacks, and the
			// filter usually lives in the same closure.
			if readsBucket && !consultsAudience {
				t.Errorf("%s: %s reads a memory bucket without consulting an audience.\n"+
					"    Every read decides who may see the result — see\n"+
					"    internal/memory/visibility.go. Scoping the vector index does\n"+
					"    not scope a function that walks a bucket directly.",
					fset.Position(fn.Pos()), fn.Name.Name)
			}
		}
	}
}
