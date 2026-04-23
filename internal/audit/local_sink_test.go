package audit

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

func newLocalSink(t *testing.T) *LocalSink {
	t.Helper()
	s, err := NewLocalSink(LocalConfig{
		Path:      filepath.Join(t.TempDir(), "audit.jsonl"),
		MaxSizeMB: 100,
		MaxFiles:  3,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func fillEntry(n int) types.AuditEntry {
	return types.AuditEntry{
		ID:         NewID(),
		Timestamp:  time.Unix(1_700_000_000+int64(n), 0).UTC(),
		ActorScope: "user:alice",
		Action:     "tool:exec",
		Target:     "bash",
	}
}

func TestLocalSinkAppendAndQuery(t *testing.T) {
	t.Parallel()
	s := newLocalSink(t)
	ctx := t.Context()

	// Three entries with a chain.
	var prev string
	for i := 0; i < 3; i++ {
		e := fillEntry(i)
		e.PrevHash = prev
		if err := s.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
		prev = ComputeHash(e)
	}

	got, err := s.Query(ctx, types.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 entries; got %d", len(got))
	}
	if got[0].Timestamp.After(got[2].Timestamp) {
		t.Error("entries should be in insertion (chronological) order")
	}
}

func TestLocalSinkQueryFiltersActor(t *testing.T) {
	t.Parallel()
	s := newLocalSink(t)
	ctx := t.Context()
	for _, scope := range []string{"user:alice", "user:bob", "user:alice"} {
		e := fillEntry(0)
		e.ActorScope = scope
		_ = s.Append(ctx, e)
	}
	got, _ := s.Query(ctx, types.AuditFilter{ActorScope: "user:alice"})
	if len(got) != 2 {
		t.Errorf("expected 2 alice entries; got %d", len(got))
	}
	for _, e := range got {
		if e.ActorScope != "user:alice" {
			t.Errorf("filter leaked: %+v", e)
		}
	}
}

func TestLocalSinkQueryLimit(t *testing.T) {
	t.Parallel()
	s := newLocalSink(t)
	ctx := t.Context()
	for i := 0; i < 10; i++ {
		_ = s.Append(ctx, fillEntry(i))
	}
	got, _ := s.Query(ctx, types.AuditFilter{Limit: 3})
	if len(got) != 3 {
		t.Errorf("limit=3 should cap at 3; got %d", len(got))
	}
}

func TestLocalSinkVerifyChainEmpty(t *testing.T) {
	t.Parallel()
	s := newLocalSink(t)
	res, err := s.VerifyChain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Errorf("empty log should verify clean")
	}
	if res.EntriesChecked != 0 {
		t.Errorf("EntriesChecked: %d", res.EntriesChecked)
	}
}

// TestLocalSinkVerifyChainClean writes three hashed entries via
// the same algorithm the log's Append uses and confirms the verify
// walk accepts them.
func TestLocalSinkVerifyChainClean(t *testing.T) {
	t.Parallel()
	s := newLocalSink(t)
	ctx := t.Context()
	var prev string
	for i := 0; i < 3; i++ {
		e := fillEntry(i)
		e.PrevHash = prev
		_ = s.Append(ctx, e)
		prev = ComputeHash(e)
	}
	res, err := s.VerifyChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Errorf("chain should verify clean; FirstBreakID=%q", res.FirstBreakID)
	}
	if res.EntriesChecked != 3 {
		t.Errorf("EntriesChecked: %d; want 3", res.EntriesChecked)
	}
}

// TestLocalSinkVerifyChainDetectsTampering writes an entry, then
// edits the file on disk to alter one entry's Action without
// recomputing hashes. Verify must report the break at the
// successor's PrevHash mismatch.
func TestLocalSinkVerifyChainDetectsTampering(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "audit.jsonl")
	s, err := NewLocalSink(LocalConfig{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	var prev string
	var ids []string
	for i := 0; i < 3; i++ {
		e := fillEntry(i)
		e.PrevHash = prev
		_ = s.Append(ctx, e)
		ids = append(ids, e.ID)
		prev = ComputeHash(e)
	}
	_ = s.Close()

	// Tamper with entry #1 (middle) — change the Action. The hash
	// stored in entry #2's PrevHash is the hash of entry #1's
	// ORIGINAL state, so the mutation creates a chain break.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	var decoded types.AuditEntry
	if err := json.Unmarshal([]byte(lines[1]), &decoded); err != nil {
		t.Fatal(err)
	}
	decoded.Action = "tool:tampered"
	tampered, _ := json.Marshal(decoded)
	lines[1] = string(tampered)
	_ = os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)

	// Re-open a sink against the same file so VerifyChain walks
	// the now-tampered log.
	s2, _ := NewLocalSink(LocalConfig{Path: path})
	t.Cleanup(func() { _ = s2.Close() })
	res, err := s2.VerifyChain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Error("tampered chain should NOT verify OK")
	}
	// Entry #2 is where the break surfaces (its PrevHash no longer
	// matches the recomputed hash of the mutated entry #1).
	if res.FirstBreakID != ids[2] {
		t.Errorf("FirstBreakID: %q; want %q", res.FirstBreakID, ids[2])
	}
}

func TestLocalSinkAppendAfterCloseReturnsErr(t *testing.T) {
	t.Parallel()
	s := newLocalSink(t)
	_ = s.Close()
	err := s.Append(t.Context(), fillEntry(0))
	if !errors.Is(err, ErrSinkClosed) {
		t.Errorf("want ErrSinkClosed; got %v", err)
	}
}

func TestLocalSinkRejectsEmptyPath(t *testing.T) {
	t.Parallel()
	_, err := NewLocalSink(LocalConfig{})
	if err == nil {
		t.Error("empty path should fail")
	}
}

// TestLocalSinkCreatesParentDir — operator didn't pre-mkdir the
// log directory; the sink creates it.
func TestLocalSinkCreatesParentDir(t *testing.T) {
	t.Parallel()
	deep := filepath.Join(t.TempDir(), "nested", "deeper", "audit.jsonl")
	s, err := NewLocalSink(LocalConfig{Path: deep})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Append(t.Context(), fillEntry(0)); err != nil {
		t.Fatalf("append into auto-created dir: %v", err)
	}
}

func TestLocalSinkName(t *testing.T) {
	t.Parallel()
	s := newLocalSink(t)
	if s.Name() != "local" {
		t.Errorf("Name: %q", s.Name())
	}
}

// --- unit tests for the internal helpers -----------------------

func TestSplitRotationBase(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ base, ext string }{
		"audit.jsonl": {"audit", ".jsonl"},
		"audit.log":   {"audit", ".log"},
		"audit":       {"audit", ""},
	}
	for in, want := range cases {
		base, ext := splitRotationBase(in)
		if base != want.base || ext != want.ext {
			t.Errorf("splitRotationBase(%q) = (%q,%q) want (%q,%q)",
				in, base, ext, want.base, want.ext)
		}
	}
}

func TestMatchesRotation(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"audit-2026-04-23T12-00-00.000.jsonl": true,
		"audit-2026.jsonl":                    true,
		"audit.jsonl":                         false, // that's the live file
		"otherfile.jsonl":                     false,
		"audit-x":                             false, // wrong extension
	}
	for name, want := range cases {
		if got := matchesRotation(name, "audit", ".jsonl"); got != want {
			t.Errorf("matchesRotation(%q) = %v; want %v", name, got, want)
		}
	}
}

func TestFilterMatcherTimeWindow(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	match := filterMatcher(types.AuditFilter{
		Since: now,
		Until: now.Add(time.Hour),
	})
	// In-window.
	if !match(types.AuditEntry{Timestamp: now.Add(30 * time.Minute)}) {
		t.Error("mid-window entry should match")
	}
	// Before.
	if match(types.AuditEntry{Timestamp: now.Add(-time.Hour)}) {
		t.Error("pre-Since entry should NOT match")
	}
	// After.
	if match(types.AuditEntry{Timestamp: now.Add(2 * time.Hour)}) {
		t.Error("post-Until entry should NOT match")
	}
}

func TestDecodeJSONLSkipsBadLines(t *testing.T) {
	t.Parallel()
	input := `{"id":"a","action":"x","ts":"2026-04-23T12:00:00Z"}
garbage not JSON
{"id":"b","action":"y","ts":"2026-04-23T12:01:00Z"}
`
	entries, err := decodeJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 parsed; got %d", len(entries))
	}
}

// TestLocalSinkChainSurvivesRotation — force a rotation by
// writing more than MaxSize's worth of entries. VerifyChain must
// walk the rotated file + the live file and report the entry count.
func TestLocalSinkChainSurvivesRotation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	// MaxSize 1MB; each entry with a long target is ~500 bytes, so
	// a few hundred entries forces a rotation.
	s, err := NewLocalSink(LocalConfig{
		Path: path, MaxSizeMB: 1, MaxFiles: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := t.Context()

	// Build a padded target so entries are larger.
	pad := strings.Repeat("x", 1024)
	var prev string
	for i := 0; i < 2000; i++ {
		e := fillEntry(i)
		e.Target = pad
		e.PrevHash = prev
		if err := s.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
		prev = ComputeHash(e)
	}

	res, err := s.VerifyChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Errorf("chain across rotation should stay clean; FirstBreakID=%q", res.FirstBreakID)
	}
	// All 2000 entries should be reachable; exact count depends on
	// rotation keeping enough backups within MaxFiles.
	if res.EntriesChecked < 500 {
		t.Errorf("expected lots of entries; got %d", res.EntriesChecked)
	}
}
