package audit

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// memSink is a minimal in-memory AuditSink used only for exercising
// the AuditLog coordinator's logic. Its behaviour is orthogonal to
// the Raft/Local sink implementations; those have their own tests.
type memSink struct {
	name    string
	entries []types.AuditEntry
	failOn  int // fail the Nth Append call (1-based). 0 disables.
	calls   int
}

func (m *memSink) Name() string { return m.name }

func (m *memSink) Append(_ context.Context, e types.AuditEntry) error {
	m.calls++
	if m.failOn != 0 && m.calls == m.failOn {
		return errors.New("injected")
	}
	m.entries = append(m.entries, e)
	return nil
}

func (m *memSink) Query(_ context.Context, filter types.AuditFilter) ([]types.AuditEntry, error) {
	match := filterMatcher(filter)
	var out []types.AuditEntry
	for _, e := range m.entries {
		if match(e) {
			out = append(out, e)
		}
	}
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (m *memSink) VerifyChain(_ context.Context) (VerifyResult, error) {
	var prev string
	var count int64
	for _, e := range m.entries {
		count++
		if e.PrevHash != prev {
			return VerifyResult{OK: false, FirstBreakID: e.ID, EntriesChecked: count}, nil
		}
		prev = ComputeHash(e)
	}
	return VerifyResult{OK: true, EntriesChecked: count}, nil
}

func TestAuditLogNoSinksIsNoOp(t *testing.T) {
	t.Parallel()
	log, err := NewAuditLog(t.Context(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(t.Context(), types.AuditEntry{
		Action: "noop",
	}); err != nil {
		t.Errorf("Append with no sinks should succeed; got %v", err)
	}
}

func TestAuditLogAppendFansOutAndChains(t *testing.T) {
	t.Parallel()
	a := &memSink{name: "a"}
	b := &memSink{name: "b"}
	log, err := NewAuditLog(t.Context(), Config{Sinks: []AuditSink{a, b}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := log.Append(t.Context(), types.AuditEntry{
			Action:    "tool:exec",
			Target:    "bash",
			Timestamp: fillEntry(i).Timestamp,
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if len(a.entries) != 3 || len(b.entries) != 3 {
		t.Fatalf("both sinks should see 3; a=%d b=%d", len(a.entries), len(b.entries))
	}
	for i := range a.entries {
		if a.entries[i].PrevHash != b.entries[i].PrevHash {
			t.Errorf("entry %d: PrevHash diverged a=%q b=%q",
				i, a.entries[i].PrevHash, b.entries[i].PrevHash)
		}
		if a.entries[i].ID != b.entries[i].ID {
			t.Errorf("entry %d: ID diverged a=%q b=%q",
				i, a.entries[i].ID, b.entries[i].ID)
		}
	}
	var prev string
	for i, e := range a.entries {
		if e.PrevHash != prev {
			t.Errorf("entry %d: PrevHash=%q want %q", i, e.PrevHash, prev)
		}
		prev = ComputeHash(e)
	}
}

// TestAuditLogAppendFillsIDAndTimestamp — coordinator populates the
// coordinator-owned fields whether the caller left them empty or not.
func TestAuditLogAppendFillsIDAndTimestamp(t *testing.T) {
	t.Parallel()
	a := &memSink{name: "a"}
	log, err := NewAuditLog(t.Context(), Config{Sinks: []AuditSink{a}})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(t.Context(), types.AuditEntry{
		ID:     "caller-supplied-should-be-replaced",
		Action: "x",
	}); err != nil {
		t.Fatal(err)
	}
	got := a.entries[0]
	if got.ID == "caller-supplied-should-be-replaced" {
		t.Error("coordinator must overwrite caller-supplied ID")
	}
	if got.ID == "" {
		t.Error("coordinator must set ID")
	}
	if got.Timestamp.IsZero() {
		t.Error("coordinator must set Timestamp when zero")
	}
}

// On sink failure the chain head must not advance, so the retry
// entry starts with the same PrevHash the failed one would have.
func TestAuditLogAppendHoldsChainHeadOnSinkError(t *testing.T) {
	t.Parallel()
	a := &memSink{name: "a", failOn: 1}
	log, err := NewAuditLog(t.Context(), Config{Sinks: []AuditSink{a}})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(t.Context(), types.AuditEntry{Action: "x"}); err == nil {
		t.Fatal("Append should have propagated the sink error")
	}
	if err := log.Append(t.Context(), types.AuditEntry{Action: "y"}); err != nil {
		t.Fatal(err)
	}
	if len(a.entries) != 1 {
		t.Fatalf("entries after retry: %d", len(a.entries))
	}
	if a.entries[0].PrevHash != "" {
		t.Errorf("retry should start fresh (PrevHash=\"\"); got %q", a.entries[0].PrevHash)
	}
}

func TestAuditLogQueryRoutesToSink(t *testing.T) {
	t.Parallel()
	a := &memSink{name: "a"}
	b := &memSink{name: "b"}
	a.entries = append(a.entries, types.AuditEntry{ID: "in-a", Action: "x"})
	b.entries = append(b.entries, types.AuditEntry{ID: "in-b", Action: "y"})
	log, _ := NewAuditLog(t.Context(), Config{Sinks: []AuditSink{a, b}})

	gotA, _ := log.Query(t.Context(), "a", types.AuditFilter{})
	if len(gotA) != 1 || gotA[0].ID != "in-a" {
		t.Errorf("Query(a) = %+v", gotA)
	}
	gotB, _ := log.Query(t.Context(), "b", types.AuditFilter{})
	if len(gotB) != 1 || gotB[0].ID != "in-b" {
		t.Errorf("Query(b) = %+v", gotB)
	}
	// Empty name → first sink.
	gotDefault, _ := log.Query(t.Context(), "", types.AuditFilter{})
	if len(gotDefault) != 1 || gotDefault[0].ID != "in-a" {
		t.Errorf("Query(default) = %+v", gotDefault)
	}
	// Unknown sink.
	if _, err := log.Query(t.Context(), "nope", types.AuditFilter{}); err == nil {
		t.Error("unknown sink should fail")
	}
}

func TestAuditLogVerifyChainAllSinks(t *testing.T) {
	t.Parallel()
	a := &memSink{name: "a"}
	b := &memSink{name: "b"}
	log, _ := NewAuditLog(t.Context(), Config{Sinks: []AuditSink{a, b}})
	for i := 0; i < 3; i++ {
		_ = log.Append(t.Context(), types.AuditEntry{Action: "x"})
	}
	res, err := log.VerifyChain(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Errorf("VerifyChain should return per-sink map; got %d entries", len(res))
	}
	for name, r := range res {
		if !r.OK {
			t.Errorf("sink %q: not clean: %+v", name, r)
		}
	}
}

func TestAuditLogVerifyChainNamedSink(t *testing.T) {
	t.Parallel()
	a := &memSink{name: "a"}
	b := &memSink{name: "b"}
	log, _ := NewAuditLog(t.Context(), Config{Sinks: []AuditSink{a, b}})
	_ = log.Append(t.Context(), types.AuditEntry{Action: "x"})

	res, err := log.VerifyChain(t.Context(), "b")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Errorf("named sink should return 1-entry map; got %d", len(res))
	}
	if _, ok := res["b"]; !ok {
		t.Errorf("expected key \"b\" in result; got %+v", res)
	}
}

// A restarted coordinator must continue the existing chain rather
// than starting fresh; otherwise every restart breaks VerifyChain.
func TestAuditLogBootAdoptsHeadHash(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	first, err := NewLocalSink(LocalConfig{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	logA, err := NewAuditLog(t.Context(), Config{Sinks: []AuditSink{first}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := logA.Append(t.Context(), types.AuditEntry{Action: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	_ = logA.Close()

	// Reopen.
	second, err := NewLocalSink(LocalConfig{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	logB, err := NewAuditLog(t.Context(), Config{Sinks: []AuditSink{second}})
	if err != nil {
		t.Fatal(err)
	}
	if err := logB.Append(t.Context(), types.AuditEntry{Action: "after-restart"}); err != nil {
		t.Fatal(err)
	}
	_ = logB.Close()

	// Reopen a third time just to verify the chain is clean.
	third, err := NewLocalSink(LocalConfig{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = third.Close() }()
	res, err := third.VerifyChain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Errorf("chain across restart should be clean; FirstBreakID=%q", res.FirstBreakID)
	}
	if res.EntriesChecked != 4 {
		t.Errorf("EntriesChecked=%d; want 4", res.EntriesChecked)
	}
}

func TestAuditLogSinks(t *testing.T) {
	t.Parallel()
	a := &memSink{name: "a"}
	b := &memSink{name: "b"}
	log, _ := NewAuditLog(t.Context(), Config{Sinks: []AuditSink{a, b}})
	got := log.Sinks()
	if len(got) != 2 {
		t.Fatalf("got %d sinks; want 2", len(got))
	}
	if got[0].Name() != "a" || got[1].Name() != "b" {
		t.Errorf("Sinks() order: %v", got)
	}
}
