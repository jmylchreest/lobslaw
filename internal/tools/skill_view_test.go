package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// Progressive disclosure, levels 1 and 2. Level 0 is the index in the
// system prompt — names, one line each, and the NAMES of bundled
// documents — which costs O(skills) and stays that way as those
// documents grow. This is what the agent calls once it has decided a
// skill is relevant.

type fakeDocs struct {
	bodies map[string]string
	refs   map[string]map[string]string
	known  map[string]bool
}

func (f fakeDocs) Has(name string) bool { return f.known[name] }

func (f fakeDocs) Body(name string) (string, bool) {
	b, ok := f.bodies[name]
	return b, ok
}

func (f fakeDocs) Reference(name, path string) (string, bool) {
	r, ok := f.refs[name][path]
	return r, ok
}

func viewBuiltin(t *testing.T, docs SkillDoc, maxBytes int) compute.BuiltinFunc {
	t.Helper()
	b := NewBuiltins()
	if err := RegisterSkillViewBuiltin(b, SkillViewConfig{Docs: docs, MaxBytes: maxBytes}); err != nil {
		t.Fatal(err)
	}
	fn, ok := b.Get("skill_view")
	if !ok {
		t.Fatal("skill_view not registered")
	}
	return fn
}

func decodeView(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return out
}

func testDocs() fakeDocs {
	return fakeDocs{
		known:  map[string]bool{"tidy": true, "bare": true},
		bodies: map[string]string{"tidy": "Run tidy before committing."},
		refs: map[string]map[string]string{
			"tidy": {"rules.md": "never reformat generated files"},
		},
	}
}

// --- level 1 -----------------------------------------------------------

func TestNameAloneReturnsTheSkillsInstructions(t *testing.T) {
	t.Parallel()
	fn := viewBuiltin(t, testDocs(), 0)
	raw, code, err := fn(context.Background(), map[string]string{"name": "tidy"})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	got := decodeView(t, raw)
	if got["content"] != "Run tidy before committing." {
		t.Errorf("content = %v", got["content"])
	}
	if got["skill"] != "tidy" {
		t.Errorf("skill = %v", got["skill"])
	}
}

// A skill with no body is ORDINARY — many are a handler and a
// description. Reporting it as a failure would teach the agent to
// avoid a tool that is working correctly.
func TestASkillWithNoBodyIsNotAnError(t *testing.T) {
	t.Parallel()
	fn := viewBuiltin(t, testDocs(), 0)
	raw, code, err := fn(context.Background(), map[string]string{"name": "bare"})
	if err != nil || code != 0 {
		t.Fatalf("a skill with no body reported a failure: code=%d err=%v", code, err)
	}
	got := decodeView(t, raw)
	note, _ := got["note"].(string)
	if !strings.Contains(note, "no instructions") {
		t.Errorf("note = %q; it does not say why the content is empty", note)
	}
}

// --- level 2 -----------------------------------------------------------

func TestNameAndPathReturnsOneBundledDocument(t *testing.T) {
	t.Parallel()
	fn := viewBuiltin(t, testDocs(), 0)
	raw, code, err := fn(context.Background(),
		map[string]string{"name": "tidy", "path": "rules.md"})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	got := decodeView(t, raw)
	if got["content"] != "never reformat generated files" {
		t.Errorf("content = %v", got["content"])
	}
	if got["path"] != "rules.md" {
		t.Errorf("path = %v; the reply does not say which document it is", got["path"])
	}
}

// --- refusals ----------------------------------------------------------

// A typo in the name and a skill that ships no documentation are
// different problems with different fixes, so they must not report the
// same way.
func TestAnUnknownSkillIsDistinguishedFromOneWithNoBody(t *testing.T) {
	t.Parallel()
	fn := viewBuiltin(t, testDocs(), 0)

	_, code, err := fn(context.Background(), map[string]string{"name": "no-such-skill"})
	if err == nil {
		t.Fatal("an unknown skill was accepted")
	}
	if code != 2 {
		t.Errorf("code = %d, want 2 — the agent picked the name and can pick another", code)
	}

	if _, code, err := fn(context.Background(), map[string]string{"name": "bare"}); err != nil || code != 0 {
		t.Errorf("a known skill with no body was reported like an unknown one: code=%d err=%v", code, err)
	}
}

func TestAnUndeclaredDocumentIsRefused(t *testing.T) {
	t.Parallel()
	fn := viewBuiltin(t, testDocs(), 0)
	_, code, err := fn(context.Background(),
		map[string]string{"name": "tidy", "path": "secrets.env"})
	if err == nil {
		t.Fatal("an undeclared document was served")
	}
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
}

func TestNameIsRequired(t *testing.T) {
	t.Parallel()
	fn := viewBuiltin(t, testDocs(), 0)
	if _, code, err := fn(context.Background(), map[string]string{}); err == nil || code != 2 {
		t.Errorf("an empty name was accepted: code=%d err=%v", code, err)
	}
}

func TestTheBuiltinNeedsADocSource(t *testing.T) {
	t.Parallel()
	if err := RegisterSkillViewBuiltin(NewBuiltins(), SkillViewConfig{}); err == nil {
		t.Error("registered with no document source")
	}
}

// --- bounds ------------------------------------------------------------

// A skill that bundles a reference manual would otherwise fill the
// context window that progressive disclosure exists to protect.
func TestALongDocumentIsTruncatedAndSaysSo(t *testing.T) {
	t.Parallel()
	docs := testDocs()
	docs.bodies["tidy"] = strings.Repeat("x", 5000)
	fn := viewBuiltin(t, docs, 100)

	raw, code, err := fn(context.Background(), map[string]string{"name": "tidy"})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	got := decodeView(t, raw)
	content, _ := got["content"].(string)
	if len(content) > 100 {
		t.Errorf("content is %d bytes, cap was 100", len(content))
	}
	// A document that stops mid-sentence otherwise reads as a document
	// that ends there.
	note, _ := got["note"].(string)
	if !strings.Contains(note, "truncated") {
		t.Errorf("note = %q; the agent cannot tell it is reading a fragment", note)
	}
}

// Cutting mid-rune produces a replacement character the model reads as
// content.
func TestTruncationCutsOnARuneBoundary(t *testing.T) {
	t.Parallel()
	docs := testDocs()
	// Three-byte runes, so a byte cap of 100 lands mid-rune.
	docs.bodies["tidy"] = strings.Repeat("★", 200)
	fn := viewBuiltin(t, docs, 100)

	raw, _, err := fn(context.Background(), map[string]string{"name": "tidy"})
	if err != nil {
		t.Fatal(err)
	}
	content, _ := decodeView(t, raw)["content"].(string)
	if strings.ContainsRune(content, '�') {
		t.Error("truncation cut mid-rune and produced a replacement character")
	}
	if content != "" && len(content)%3 != 0 {
		t.Errorf("content is %d bytes; not a whole number of 3-byte runes", len(content))
	}
}

// A document exactly at the cap is not truncated, or every document
// would carry a note saying it might be incomplete.
func TestADocumentExactlyAtTheCapIsNotTruncated(t *testing.T) {
	t.Parallel()
	docs := testDocs()
	docs.bodies["tidy"] = strings.Repeat("x", 100)
	fn := viewBuiltin(t, docs, 100)

	raw, _, err := fn(context.Background(), map[string]string{"name": "tidy"})
	if err != nil {
		t.Fatal(err)
	}
	if note, _ := decodeView(t, raw)["note"].(string); note != "" {
		t.Errorf("note = %q for a document that fits exactly", note)
	}
}
