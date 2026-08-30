package compute

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/notify"
	"github.com/jmylchreest/lobslaw/internal/turn"
)

// identityArgKeys are the keys that once carried caller identity in the
// tool-argument map. They are listed here so the guard below fails on
// the historical names specifically, rather than on any "__" key — a
// future synthetic argument that is genuinely not identity (a
// pagination cursor, say) should not have to fight this test.
var identityArgKeys = []string{
	"__user_id",
	"__chat_id",
	"__channel",
	"__scope",
	"__user_timezone",
}

// TestBuiltinsDoNotReadIdentityFromArgs is the guard on the invariant,
// as opposed to the tests below which guard instances of it.
//
// Caller identity reaches a builtin on the context, never through the
// argument map, because that map is built from the model's own JSON.
// The original bug was not that one handler read the wrong key: it was
// that trusted and untrusted values shared a namespace, so every new
// handler was one plausible line away from reintroducing it. A test
// that enumerates today's handlers cannot catch tomorrow's. This reads
// the package's own source and fails on the shape.
func TestBuiltinsDoNotReadIdentityFromArgs(t *testing.T) {
	t.Parallel()

	// turn_identity.go documents the retired keys in prose, and this
	// file names them in identityArgKeys; both are the fix, not the
	// bug.
	exempt := map[string]bool{
		"turn_identity.go":      true,
		"turn_identity_test.go": true,
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no source files found — is this test running outside the package directory?")
	}

	banned := make(map[string]bool, len(identityArgKeys))
	for _, k := range identityArgKeys {
		banned[k] = true
	}

	fset := token.NewFileSet()
	for _, path := range files {
		if exempt[filepath.Base(path)] {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// Parsed rather than grepped so a key named in a comment — the
		// explanation of why this rule exists — is not itself a
		// violation.
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			idx, ok := n.(*ast.IndexExpr)
			if !ok {
				return true
			}
			lit, ok := idx.Index.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			key, err := strconv.Unquote(lit.Value)
			if err != nil || !banned[key] {
				return true
			}
			t.Errorf("%s: reads caller identity %q out of a map.\n"+
				"    Identity comes from turn.IdentityFrom(ctx). The argument map is\n"+
				"    populated from the model's output, so a value read from it is a\n"+
				"    value the model chose — see internal/compute/turn_identity.go.",
				fset.Position(idx.Pos()), key)
			return true
		})
	}
}

// recordingNotifier captures what notify would have sent.
type recordingNotifier struct{ last notify.Notification }

func (r *recordingNotifier) Send(_ context.Context, n notify.Notification) error {
	r.last = n
	return nil
}

// TestNotifyIgnoresModelSuppliedIdentity is the instance that mattered
// most: notify reaches every device a user has bound, so a model that
// can name the recipient can message anyone the node knows.
func TestNotifyIgnoresModelSuppliedIdentity(t *testing.T) {
	t.Parallel()
	rec := &recordingNotifier{}
	h := newNotifyHandler(rec)

	ctx := turn.WithIdentity(context.Background(), turn.Identity{
		UserID:    "alice",
		Channel:   "telegram",
		ChannelID: "100",
	})
	// Every key here is the model asking to be someone else.
	if _, _, err := h(ctx, map[string]string{
		"text":      "hello",
		"__user_id": "bob",
		"__channel": "rest",
		"__chat_id": "999",
	}); err != nil {
		t.Fatal(err)
	}

	if rec.last.UserID != "alice" {
		t.Errorf("notified %q; the model picked the recipient", rec.last.UserID)
	}
	if rec.last.OriginatorChannel != "telegram" || rec.last.OriginatorID != "100" {
		t.Errorf("originator = %s:%s; want telegram:100",
			rec.last.OriginatorChannel, rec.last.OriginatorID)
	}
}

// An explicit user_id remains a legitimate routing argument — "tell
// bob his build finished" — so the guard above must not have closed it.
func TestNotifyStillHonoursExplicitUserID(t *testing.T) {
	t.Parallel()
	rec := &recordingNotifier{}
	h := newNotifyHandler(rec)

	ctx := turn.WithIdentity(context.Background(), turn.Identity{UserID: "alice"})
	if _, _, err := h(ctx, map[string]string{"text": "hi", "user_id": "bob"}); err != nil {
		t.Fatal(err)
	}
	if rec.last.UserID != "bob" {
		t.Errorf("notified %q; want the explicitly addressed bob", rec.last.UserID)
	}
}

// A turn with no identity has nobody to fall back on, and must say so
// rather than silently notifying whoever the model names.
func TestNotifyWithoutIdentityRefusesRatherThanGuessing(t *testing.T) {
	t.Parallel()
	rec := &recordingNotifier{}
	h := newNotifyHandler(rec)

	_, code, err := h(context.Background(), map[string]string{
		"text":      "hello",
		"__user_id": "bob",
	})
	if err == nil {
		t.Fatalf("notify succeeded with no caller identity; sent to %q", rec.last.UserID)
	}
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (bad request)", code)
	}
}

func TestIdentityArgGuardDetectsAViolation(t *testing.T) {
	t.Parallel()
	const bad = `package p
func h(args map[string]string) string { return args["__user_id"] }`

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "bad.go", bad, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	ast.Inspect(file, func(n ast.Node) bool {
		idx, ok := n.(*ast.IndexExpr)
		if !ok {
			return true
		}
		lit, ok := idx.Index.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		key, _ := strconv.Unquote(lit.Value)
		if strings.HasPrefix(key, "__") {
			found = true
		}
		return true
	})
	if !found {
		t.Error("the AST shape this guard relies on did not match a known violation")
	}
}
