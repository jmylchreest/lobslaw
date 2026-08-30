package compute

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A builtin and its ToolDef are paired by STRING NAME and by nothing
// else. b.Register("x", fn) puts the handler in one map;
// types.ToolDef{Name: "x"} puts the description in another. Nothing
// checks they agree.
//
// Both halves of getting that wrong are silent:
//
//   - a builtin with no ToolDef is invisible to the model. It exists,
//     it works, and nothing will ever call it.
//   - a ToolDef with no builtin is worse: the model reads it, calls
//     it, and gets "not found" — a tool that advertises itself and
//     then fails.
//
// Source-level because the two registrations happen in different
// packages at different wiring stages, and no single runtime moment
// holds both complete. This is the cheapest thing that would notice.
func TestEveryBuiltinHasAToolDefAndViceVersa(t *testing.T) {
	t.Parallel()

	registered := map[string]string{} // name -> file
	described := map[string]string{}

	reRegister := regexp.MustCompile(`\bb\.Register\("([a-z_0-9]+)"`)
	reDef := regexp.MustCompile(`Name:\s+"([a-z_0-9]+)"`)
	// The debug tools are built through a helper rather than inline
	// literals; matched separately so they are not reported as missing.
	reMk := regexp.MustCompile(`\bmk\("([a-z_0-9]+)"`)

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatal(rerr)
		}
		body := string(src)
		for _, m := range reRegister.FindAllStringSubmatch(body, -1) {
			registered[m[1]] = f
		}
		for _, re := range []*regexp.Regexp{reDef, reMk} {
			for _, m := range re.FindAllStringSubmatch(body, -1) {
				described[m[1]] = f
			}
		}
	}

	if len(registered) == 0 || len(described) == 0 {
		t.Fatal("scanned nothing; the patterns no longer match how tools are declared")
	}

	for name, file := range registered {
		if _, ok := described[name]; !ok {
			t.Errorf("%s registers builtin %q with no ToolDef — the model can never call it", file, name)
		}
	}
	for name, file := range described {
		if _, ok := registered[name]; !ok {
			t.Errorf("%s declares ToolDef %q with no builtin — the model will call it and get \"not found\"", file, name)
		}
	}
}

// Every cross-reference must name a tool that EXISTS SOMEWHERE in the
// codebase, even if a given deployment disables it.
//
// The distinction is the whole design. Filtering happens at runtime
// against what is REGISTERED, so a disabled tool is silently dropped
// from a recommendation and that is correct. A name that exists
// nowhere is a different thing: a typo, which filtering hides forever
// because it drops out exactly like a disabled tool would.
func TestCrossReferencesNameRealTools(t *testing.T) {
	t.Parallel()

	known := map[string]bool{}
	type ref struct{ from, to, file string }
	var refs []ref

	reDef := regexp.MustCompile(`Name:\s+"([a-z_0-9]+)"`)
	reMk := regexp.MustCompile(`\bmk\("([a-z_0-9]+)"`)
	reList := regexp.MustCompile(`(RecommendTools|AvoidTools):\s*\[\]string\{([^}]*)\}`)
	reName := regexp.MustCompile(`"([a-z_0-9]+)"`)

	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		body := string(src)
		for _, re := range []*regexp.Regexp{reDef, reMk} {
			for _, m := range re.FindAllStringSubmatch(body, -1) {
				known[m[1]] = true
			}
		}
		for _, m := range reList.FindAllStringSubmatch(body, -1) {
			for _, n := range reName.FindAllStringSubmatch(m[2], -1) {
				refs = append(refs, ref{from: m[1], to: n[1], file: f})
			}
		}
	}

	if len(refs) == 0 {
		t.Skip("no cross-references declared yet")
	}
	for _, r := range refs {
		if !known[r.to] {
			t.Errorf("%s: %s names %q, which is not a tool anywhere — "+
				"runtime filtering would drop it silently, exactly like a disabled tool",
				r.file, r.from, r.to)
		}
	}
}
