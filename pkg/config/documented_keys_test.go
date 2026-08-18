package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The mirror of TestEverySettingIsReadBySomething.
//
// That one catches a setting nothing reads. This catches a setting
// that does not exist: a key the reference teaches which no struct
// field claims. koanf ignores unknown keys silently, so following the
// documentation produces DEFAULTS with no warning — and the operator
// has no way to tell their configuration was discarded.
//
// The `[cluster]` block shipped with four such keys. `node_id` was
// even marked "required" and is not a key at all; `listen_address` is
// spelled `listen_addr`, so every documented cluster listener silently
// bound the default port. Found by running a node, not by reading it.

var tomlKeyRe = regexp.MustCompile(`(?m)^([a-z_0-9]+)\s*=`)

// knownWrongInTheReference is documentation debt, enumerated so it is
// visible rather than absent.
//
// Every entry is a key the reference teaches which no struct field
// claims: koanf discards it silently, so an operator writing it gets
// the default and no warning. Each needs triage that this test cannot
// do — renamed, moved to another section, or documenting a feature
// that was removed — and guessing wholesale would replace wrong
// documentation with differently wrong documentation.
//
// The list only shrinks. A NEW phantom key fails immediately, which is
// the point: this stops the drift getting worse while the backlog is
// worked through.
// knownWrongInTheReference is documentation debt.
//
// Empty, and it should stay that way. Every entry would be a key the
// reference teaches which no struct field claims: koanf discards it
// silently, so an operator writing it gets the default and no warning.
var knownWrongInTheReference = map[string]bool{}

func TestEveryDocumentedKeyExists(t *testing.T) {
	t.Parallel()
	documented := documentedTOMLKeys(t)
	if len(documented) < 20 {
		t.Fatalf("only %d documented keys parsed; the reader has drifted from the docs "+
			"and is no longer checking anything", len(documented))
	}

	known := koanfTags(t)
	for _, key := range documented {
		if knownWrongInTheReference[key] {
			continue
		}
		if !known[key] {
			t.Errorf("`%s` appears in the configuration reference but no struct field "+
				"carries koanf:%q — koanf ignores it silently, so anyone following the "+
				"docs gets the default", key, key)
		}
	}
}

// documentedTOMLKeys pulls every `key =` out of the reference's fenced
// toml blocks.
//
// Flat rather than section-aware: a key correct in one section and
// wrong in another would slip through, but the failure this exists to
// catch — a key that exists NOWHERE — does not need the extra
// machinery, and section tracking across nested tables would be its
// own source of false alarms.
func documentedTOMLKeys(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "docs", "configuration", "reference.md"))
	if err != nil {
		t.Skipf("configuration reference not readable: %v", err)
	}
	var out []string
	seen := map[string]bool{}
	inBlock := false
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "```toml"):
			inBlock = true
			continue
		case strings.HasPrefix(line, "```"):
			inBlock = false
			continue
		}
		if !inBlock || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, m := range tomlKeyRe.FindAllStringSubmatch(line, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
	}
	return out
}

// koanfTags collects every koanf tag declared in this package.
//
// Read from the source rather than by reflecting over Config, because
// reflection would miss a type that is declared but not yet reachable
// from the root — and one of those is exactly where a documented key
// would hide.
func koanfTags(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		{
			file, perr := parser.ParseFile(fset, e.Name(), nil, 0)
			if perr != nil {
				t.Fatalf("parse %s: %v", e.Name(), perr)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				st, ok := n.(*ast.StructType)
				if !ok {
					return true
				}
				for _, f := range st.Fields.List {
					if f.Tag == nil {
						continue
					}
					tag := reflect.StructTag(strings.Trim(f.Tag.Value, "`"))
					if v, has := tag.Lookup("koanf"); has {
						out[strings.Split(v, ",")[0]] = true
					}
				}
				return true
			})
		}
	}
	return out
}
