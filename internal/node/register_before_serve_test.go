package node

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Start() calls Serve immediately, and gRPC makes RegisterService
// after Serve a FATAL — it kills the process rather than returning an
// error, so nothing downstream can catch it or report it well.
//
// SkillService was registered in startSkillStoreLoader, which runs
// after Serve. It never fired, because the step before it refused a
// relative data_dir and returned early on every documented config.
// Fixing that path made the registration reachable and the node died
// on boot: the crash had been latent behind an unrelated failure.
//
// This is an ordering invariant, and a runtime test cannot assert it —
// the failure mode is process death. So it is asserted on the source:
// nothing reachable from Start may register a gRPC service.
func TestNothingReachableFromStartRegistersAGRPCService(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	files := parsePackage(t, fset)
	bodies := map[string]*ast.FuncDecl{}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			bodies[fn.Name.Name] = fn
		}
	}

	start, ok := bodies["Start"]
	if !ok {
		t.Fatal("no Start method found; this test is looking at the wrong package")
	}

	// Walk outward from Start through method calls on the node.
	seen := map[string]bool{}
	queue := []*ast.FuncDecl{start}
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		if seen[fn.Name.Name] {
			continue
		}
		seen[fn.Name.Name] = true

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch f := call.Fun.(type) {
			case *ast.SelectorExpr:
				name := f.Sel.Name
				// Only the SHARED server matters. A function that
				// builds its own grpc.Server and registers on it
				// before calling Serve — the enrolment listener does
				// exactly that on its own port — is correct, and
				// flagging it would train people to ignore this test.
				if strings.HasPrefix(name, "Register") && strings.HasSuffix(name, "ServiceServer") &&
					len(call.Args) > 0 && isNodeServer(call.Args[0]) {
					t.Errorf("%s is reachable from Start and calls %s on n.server; "+
						"gRPC treats RegisterService after Serve as FATAL — "+
						"register it during wiring instead",
						fn.Name.Name, name)
				}
				// A call on the node itself: follow it.
				if next, ok := bodies[name]; ok {
					queue = append(queue, next)
				}
			case *ast.Ident:
				if next, ok := bodies[f.Name]; ok {
					queue = append(queue, next)
				}
			}
			return true
		})
	}

	// A guard on the guard: if the walk found nothing, the traversal
	// is broken and this test is green for the wrong reason.
	if len(seen) < 5 {
		t.Errorf("only walked %d functions from Start (%v); the traversal is not reaching the startup sequence",
			len(seen), seen)
	}
}

// isNodeServer reports whether e is the node's shared gRPC server,
// the one Start() has already handed to Serve.
func isNodeServer(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "server" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "n"
}

// parsePackage reads this package's non-test sources.
//
// Walked file by file rather than with parser.ParseDir, which is
// deprecated for not honouring build tags.
func parsePackage(t *testing.T, fset *token.FileSet) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var out []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		t.Fatal("parsed no source files; this test would pass vacuously")
	}
	return out
}
