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
	assertCalledOnce(t, "n.embedder assignment", func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return false
		}
		for _, lhs := range assign.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "embedder" {
				continue
			}
			if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "n" {
				return true
			}
		}
		return false
	})
}

// The model-change guard must run for EVERY embedder kind, so it is
// called exactly once — at the call site, not inside a branch.
//
// It was inside wireEmbedder's remote branch. When the builtin branch
// was added it skipped the guard entirely, and the builtin path is the
// one where changing models is easy: a line of config rather than a new
// API key. A node then loaded a different model over a corpus written
// by the old one and started perfectly happily, which is the exact
// failure the guard exists to prevent.
func TestTheEmbeddingModelGuardRunsExactlyOnce(t *testing.T) {
	assertCalledOnce(t, "memory.CheckEmbeddingModel call", func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "CheckEmbeddingModel" {
			return false
		}
		ident, ok := sel.X.(*ast.Ident)
		return ok && ident.Name == "memory"
	})
}

// assertCalledOnce parses every non-test file in the package and fails
// unless match hits exactly once.
func assertCalledOnce(t *testing.T, what string, match func(ast.Node) bool) {
	t.Helper()
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var sites []string
	var files int
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files++
		ast.Inspect(f, func(n ast.Node) bool {
			if n != nil && match(n) {
				sites = append(sites, fset.Position(n.Pos()).String())
			}
			return true
		})
	}
	if files < 5 {
		t.Fatalf("only parsed %d files; the glob is not finding the package", files)
	}
	if len(sites) != 1 {
		t.Errorf("%s appears in %d places, want exactly 1:\n  %s",
			what, len(sites), strings.Join(sites, "\n  "))
	}
}
