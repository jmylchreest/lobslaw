package node

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// n.embedder MUST be assigned exactly once, at the call site.
//
// It was assigned inside the remote branch of wireEmbedder. When the
// builtin branch was added it returned a perfectly good provider and
// left the field nil, so memory_search — which reads the RETURN VALUE —
// got an embedder while the episodic ingester and the context engine —
// which read the FIELD — did not. The node logged "builtin embedding
// model ready" and then wrote no vectors at all.
//
// Nothing failed. No error, no warning; recall simply stayed lexical on
// a node configured for semantic search, and only reading the store
// afterwards showed it. Every unit test passed throughout.
//
// One assignment means a third embedder kind cannot repeat it.
func TestTheEmbedderIsAssignedExactlyOnce(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	// Every .go file, parsed individually rather than through
	// parser.ParseDir. ParseDir is deprecated, but the better reason is
	// that it does not consider build tags — and a build-tagged file
	// assigning n.embedder is exactly as dangerous as an untagged one,
	// so this guard must see all of them.
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	files := make([]*ast.File, 0, len(paths))
	names := make([]string, 0, len(paths))
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files = append(files, f)
		names = append(names, path)
	}
	if len(files) < 5 {
		t.Fatalf("only parsed %d files; the glob is not finding the package", len(files))
	}

	var sites []string
	for i, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range assign.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "embedder" {
					continue
				}
				if ident, ok := sel.X.(*ast.Ident); !ok || ident.Name != "n" {
					continue
				}
				sites = append(sites, names[i]+":"+fset.Position(assign.Pos()).String())
			}
			return true
		})
	}

	if len(sites) != 1 {
		t.Errorf("n.embedder is assigned in %d places, want exactly 1 (the call site in wireEmbedder's caller):\n  %s",
			len(sites), strings.Join(sites, "\n  "))
	}
}
