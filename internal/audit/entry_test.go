package audit

import (
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestComputeHashDeterministic(t *testing.T) {
	t.Parallel()
	e := types.AuditEntry{
		Timestamp:  time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC),
		ActorScope: "user:alice",
		Action:     "tool:exec",
		Target:     "bash",
		Argv:       []string{"ls", "-la"},
		PolicyRule: "rule-1",
		Effect:     types.Effect("allow"),
		ResultHash: "abc",
		PrevHash:   "prev",
	}
	h1 := ComputeHash(e)
	h2 := ComputeHash(e)
	if h1 != h2 {
		t.Errorf("hash not deterministic: %q vs %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("hash should be 64 hex chars; got %d", len(h1))
	}
}

func TestComputeHashChangesOnMutation(t *testing.T) {
	t.Parallel()
	base := types.AuditEntry{
		Timestamp: time.Unix(1_700_000_000, 0).UTC(),
		Action:    "tool:exec",
		Target:    "ls",
	}
	orig := ComputeHash(base)

	mutations := []func(*types.AuditEntry){
		func(e *types.AuditEntry) { e.ActorScope = "user:mallory" },
		func(e *types.AuditEntry) { e.Action = "tool:delete" },
		func(e *types.AuditEntry) { e.Target = "different" },
		func(e *types.AuditEntry) { e.Argv = []string{"--sneaky"} },
		func(e *types.AuditEntry) { e.PolicyRule = "different-rule" },
		func(e *types.AuditEntry) { e.Effect = "deny" },
		func(e *types.AuditEntry) { e.ResultHash = "abc123" },
		func(e *types.AuditEntry) { e.PrevHash = "different-prev" },
	}
	for i, mutate := range mutations {
		variant := base
		mutate(&variant)
		if h := ComputeHash(variant); h == orig {
			t.Errorf("mutation %d didn't change hash", i)
		}
	}
}

// TestComputeHashIgnoresID — ID is a ULID, not data. Including it
// in the hash would let an attacker fabricate a consistent chain
// after removing/rewriting entries; excluding it means any removal
// shows up as a PrevHash mismatch on the successor.
func TestComputeHashIgnoresID(t *testing.T) {
	t.Parallel()
	a := types.AuditEntry{ID: "aaaa", Timestamp: time.Unix(1, 0).UTC(), Action: "x"}
	b := types.AuditEntry{ID: "bbbb", Timestamp: time.Unix(1, 0).UTC(), Action: "x"}
	if ComputeHash(a) != ComputeHash(b) {
		t.Error("hash should be ID-independent")
	}
}

func TestNewIDLooksLikeULID(t *testing.T) {
	t.Parallel()
	id := NewID()
	if len(id) != 26 {
		t.Errorf("ULID should be 26 chars; got %d (%q)", len(id), id)
	}
	// ULIDs are base32 Crockford — no "I", "L", "O", "U".
	for _, c := range strings.ToUpper(id) {
		if c == 'I' || c == 'L' || c == 'O' || c == 'U' {
			t.Errorf("non-Crockford character in ULID: %q", id)
		}
	}
}

func TestNewIDMonotonic(t *testing.T) {
	t.Parallel()
	// Two IDs generated back-to-back sort so the second >= first —
	// ULID timestamp + monotonic counter guarantees this.
	a := NewID()
	b := NewID()
	if a > b {
		t.Errorf("ULIDs not monotonic: %q > %q", a, b)
	}
}

func TestValidateEntryRequiresTimestamp(t *testing.T) {
	t.Parallel()
	err := ValidateEntry(types.AuditEntry{Action: "x"})
	if err == nil || !strings.Contains(err.Error(), "Timestamp") {
		t.Errorf("missing timestamp should fail with a helpful message; got %v", err)
	}
}

func TestValidateEntryRequiresAction(t *testing.T) {
	t.Parallel()
	err := ValidateEntry(types.AuditEntry{Timestamp: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "Action") {
		t.Errorf("missing action should fail; got %v", err)
	}
}

func TestValidateEntryHappyPath(t *testing.T) {
	t.Parallel()
	err := ValidateEntry(types.AuditEntry{
		Timestamp: time.Now(),
		Action:    "tool:exec",
	})
	if err != nil {
		t.Errorf("minimal valid entry should pass: %v", err)
	}
}
