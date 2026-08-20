package node

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
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
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	var sites []string
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
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
					sites = append(sites, name+":"+fset.Position(assign.Pos()).String())
				}
				return true
			})
		}
	}
	if len(sites) != 1 {
		t.Errorf("n.embedder is assigned in %d places, want exactly 1 (the call site in wireEmbedder's caller):\n  %s",
			len(sites), strings.Join(sites, "\n  "))
	}
}
