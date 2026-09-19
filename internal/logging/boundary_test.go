package logging

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Keep future production constructors from silently creating another output
// pipeline. Discard-only loggers and the dependency adapter are explicit seams.
func TestProductionLoggingBoundaries(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	for _, dir := range []string{"cmd/lobslaw", "internal", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".pb.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if strings.Contains(rel, "/internal/goldengen/") {
				return nil
			}
			set := token.NewFileSet()
			f, err := parser.ParseFile(set, path, nil, 0)
			if err != nil {
				return err
			}
			imports := map[string]string{}
			for _, im := range f.Imports {
				pkg, _ := strconv.Unquote(im.Path.Value)
				name := filepath.Base(pkg)
				if im.Name != nil {
					name = im.Name.Name
				}
				imports[name] = pkg
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				allowed := permittedLoggingCall(imports[id.Name], sel.Sel.Name, rel, call)
				if !allowed {
					t.Errorf("%s: use the shared logging pipeline", set.Position(call.Pos()))
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func permittedLoggingCall(pkg, name, rel string, call *ast.CallExpr) bool {
	allowed := true
	switch pkg {
	case "github.com/jmylchreest/slog-logfilter":
		if name == "New" {
			allowed = rel == "internal/logging/log.go"
		}
	case "github.com/sirupsen/logrus":
		if name == "New" || name == "SetOutput" {
			allowed = rel == "internal/egress/logging.go"
		}
	case "log/slog":
		switch name {
		case "NewJSONHandler", "NewTextHandler", "NewLogLogger":
			allowed = strings.HasPrefix(rel, "internal/logging/")
		case "New":
			allowed = rel == "internal/logging/sanitize.go"
			if len(call.Args) == 1 {
				if a, ok := call.Args[0].(*ast.SelectorExpr); ok && a.Sel.Name == "DiscardHandler" {
					allowed = true
				}
			}
		case "SetDefault":
			allowed = rel == "cmd/lobslaw/main.go"
		}
	case "log":
		if name == "New" || name == "SetOutput" {
			allowed = strings.HasPrefix(rel, "internal/logging/") || (name == "New" && rel == "internal/memory/raft_log.go")
		}
	case "flag":
		if name == "NewFlagSet" {
			allowed = rel == "cmd/lobslaw/logging.go"
		}
	}
	return allowed
}
