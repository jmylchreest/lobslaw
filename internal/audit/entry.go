package audit

import (
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// ComputeHash returns the hex-encoded SHA-256 of the entry's
// canonical serialisation. Inputs are concatenated in fixed order;
// ID is excluded so a chain break stays visible even if an attacker
// rewrites IDs. Delimiter is U+001F (unit separator) — text-heavy
// payloads essentially never contain it, so a "|" delimiter
// colliding with argv pipes isn't a concern.
func ComputeHash(e types.AuditEntry) string {
	const sep = "\x1f"
	parts := []string{
		e.Timestamp.UTC().Format(time.RFC3339Nano),
		e.ActorScope,
		e.Action,
		e.Target,
		strings.Join(e.Argv, sep),
		e.PolicyRule,
		string(e.Effect),
		e.ResultHash,
		e.PrevHash,
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, sep)))
	return hex.EncodeToString(sum[:])
}

// Shared monotonic entropy source. Two IDs generated within the same
// millisecond must sort monotonically, which requires them to share a
// single ulid.MonotonicReader — a fresh source per call resets the
// monotonic counter and can produce out-of-order IDs. Guarded by a
// mutex because ulid.MonotonicReader is not safe for concurrent use.
var (
	idEntropyMu sync.Mutex
	idEntropy   = ulid.Monotonic(cryptorand.Reader, 0)
)

// NewID returns a fresh ULID for an audit entry. ULIDs sort
// lexicographically by creation time and are safe as map keys +
// JSONL cursor positions. 26 characters base-32.
func NewID() string {
	idEntropyMu.Lock()
	defer idEntropyMu.Unlock()
	return ulid.MustNew(ulid.Now(), idEntropy).String()
}

// ValidateEntry sanity-checks an entry before Append processes it.
// The Timestamp and Action are required; everything else is
// operator-meaningful but not fatal if empty (e.g. ResultHash is
// only set when the action produced output).
func ValidateEntry(e types.AuditEntry) error {
	if e.Timestamp.IsZero() {
		return fmt.Errorf("audit: entry.Timestamp is required")
	}
	if e.Action == "" {
		return fmt.Errorf("audit: entry.Action is required")
	}
	return nil
}
