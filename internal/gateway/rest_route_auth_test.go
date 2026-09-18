package gateway

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// restRouteClass is how a mounted path is authorised. The table is
// the classification; the source scan is what stops a new HandleFunc
// shipping without one.
type restRouteClass int

const (
	restRoutePublic restRouteClass = iota
	restRouteOwnAuth
	restRouteUserData
)

// restRouteAuthTable classifies every REST path this package mounts.
// Adding a mux.HandleFunc without a row here fails
// TestEveryMountedPathIsInTheAuthTable. Adding a user-data row
// without calling authenticate fails
// TestUnauthenticatedCallerGets401OnEveryUserDataRoute.
func restRouteAuthTable() map[string]restRouteClass {
	return map[string]restRouteClass{
		"/":                restRoutePublic,
		"/healthz":         restRoutePublic,
		"/readyz":          restRoutePublic,
		"/telegram":        restRouteOwnAuth,
		"/v1/messages":     restRouteUserData,
		"/v1/plan":         restRouteUserData,
		"/v1/prompts/":     restRouteUserData,
		"/v1/capabilities": restRouteUserData,
		"/v1/session":      restRouteUserData,
		"/v1/session/code": restRouteOwnAuth,
		"/v1/bots":         restRouteUserData,
		"/v1/bots/":        restRouteUserData,
		"/v1/groups":       restRouteUserData,
		"/v1/groups/":      restRouteUserData,
		"/v1/inbox/":       restRouteUserData,
		"/v1/activity":     restRouteUserData,
		"/webhook/":        restRouteOwnAuth,
	}
}

func TestUnauthenticatedCallerGets401OnEveryUserDataRoute(t *testing.T) {
	t.Parallel()
	srv := startWebREST(t, &captureRunner{}, nil)
	base := webBaseURL(srv)

	// Concrete requests, not a six-path hand list: every user-data
	// classification is probed. RequireAuth is on — without it
	// authenticate returns anon by design and this would pass while
	// the routes were open.
	probes := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/v1/messages", `{"message":"hi"}`},
		{http.MethodGet, "/v1/plan", ""},
		{http.MethodGet, "/v1/prompts/p1", ""},
		{http.MethodPost, "/v1/prompts/p1/resolve", `{"approve":true}`},
		{http.MethodGet, "/v1/capabilities", ""},
		{http.MethodGet, "/v1/session", ""},
		{http.MethodDelete, "/v1/session", ""},
		{http.MethodPost, "/v1/session", ""},
	}
	for _, p := range probes {
		t.Run(p.method+" "+p.path, func(t *testing.T) {
			resp := doJSON(t, p.method, base+p.path, p.body, nil)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s = %d, want 401 — this route is open",
					p.method, p.path, resp.StatusCode)
			}
		})
	}
}

func TestPublicRoutesStayReachableWithoutAuth(t *testing.T) {
	t.Parallel()
	srv := startWebREST(t, &captureRunner{}, nil)
	base := webBaseURL(srv)

	resp := doJSON(t, http.MethodGet, base+"/healthz", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d, want 200", resp.StatusCode)
	}
}

func TestRESTPlanGatedWhenRequireAuth(t *testing.T) {
	t.Parallel()
	srv := startWebREST(t, nil, nil)
	resp := doJSON(t, http.MethodGet, webBaseURL(srv)+"/v1/plan", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /v1/plan without credentials = %d, want 401", resp.StatusCode)
	}
}

// Every string literal passed to mux.Handle / mux.HandleFunc in this
// package must appear in restRouteAuthTable. A new route that is not
// in the table cannot be classified, so it cannot be shown to gate.
func TestEveryMountedPathIsInTheAuthTable(t *testing.T) {
	t.Parallel()
	table := restRouteAuthTable()
	mounted := muxPathLiterals(t)
	if len(mounted) == 0 {
		t.Fatal("source scan found no mux path literals")
	}
	for _, path := range mounted {
		if _, ok := table[path]; ok {
			continue
		}
		classified := false
		for prefix := range table {
			if strings.HasPrefix(path, prefix) {
				classified = true
				break
			}
		}
		if !classified {
			t.Errorf("mux mounts %q but restRouteAuthTable does not classify it — ungated routes fail CI", path)
		}
	}
}

func muxPathLiterals(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var out []string
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle" {
				return true
			}
			if len(call.Args) == 0 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			path, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if !seen[path] {
				seen[path] = true
				out = append(out, path)
			}
			return true
		})
	}
	return out
}
