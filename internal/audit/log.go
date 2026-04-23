package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// AuditLog is the single writer surface the rest of lobslaw uses.
// Coordinates writes across N sinks under a single mutex so every
// sink sees the same entries in the same order with the same
// PrevHash chain — a cross-sink comparison at VerifyChain time
// detects tampering on either side.
type AuditLog struct {
	sinks []AuditSink
	log   *slog.Logger

	mu   sync.Mutex
	prev string // head hash shared across sinks
}

// Config bundles sinks + optional logger. Empty sinks is valid —
// an AuditLog with no backends silently no-ops Append; used in
// single-node deployments where the operator explicitly disabled
// both raft and local audit.
type Config struct {
	Sinks  []AuditSink
	Logger *slog.Logger
}

// NewAuditLog constructs the coordinator. On construction, it
// probes every sink for its last entry (via a Query with no
// filter, reversed) so the PrevHash chain continues across process
// restarts. If sinks disagree (different head hashes), we pick the
// one from the first sink and log a warning — a mid-chain split is
// detectable by VerifyChain anyway.
func NewAuditLog(ctx context.Context, cfg Config) (*AuditLog, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	a := &AuditLog{
		sinks: cfg.Sinks,
		log:   logger,
	}
	if len(cfg.Sinks) == 0 {
		return a, nil
	}

	// Read the last entry from the first sink and adopt its hash
	// as our starting prev. Other sinks' heads are reported
	// separately so divergence is visible.
	head, err := lastEntry(ctx, cfg.Sinks[0])
	if err != nil {
		return nil, fmt.Errorf("audit: read head of %q: %w", cfg.Sinks[0].Name(), err)
	}
	if head != nil {
		a.prev = ComputeHash(*head)
	}
	for _, sink := range cfg.Sinks[1:] {
		peer, err := lastEntry(ctx, sink)
		if err != nil {
			logger.Warn("audit: read head failed",
				"sink", sink.Name(), "err", err)
			continue
		}
		if peer == nil && head == nil {
			continue
		}
		if peer == nil || head == nil || ComputeHash(*peer) != a.prev {
			logger.Warn("audit: sinks start with divergent head — VerifyChain will show extent",
				"primary", cfg.Sinks[0].Name(),
				"divergent", sink.Name())
		}
	}
	return a, nil
}

// Append fills in coordinator-owned fields (ID, Timestamp when
// zero, PrevHash) and fans out to every sink. If any sink errors,
// Append returns that error immediately — earlier sinks will have
// been written, which is acceptable: partial writes are how
// defence-in-depth pairs should fail (one sink down shouldn't
// silently skip the other). The caller retries; a restart re-reads
// the head from the first sink.
func (a *AuditLog) Append(ctx context.Context, entry types.AuditEntry) error {
	_, err := a.AppendEntry(ctx, entry)
	return err
}

// AppendEntry is like Append but returns the enriched entry (with
// coordinator-filled ID, Timestamp, PrevHash). Callers that need
// the assigned ID — notably the gRPC surface, which echoes it back
// in AppendResponse — use this variant to avoid an extra Query.
func (a *AuditLog) AppendEntry(ctx context.Context, entry types.AuditEntry) (types.AuditEntry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	entry.ID = NewID()
	entry.PrevHash = a.prev
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}
	if err := ValidateEntry(entry); err != nil {
		return types.AuditEntry{}, err
	}
	newHead := ComputeHash(entry)

	for _, sink := range a.sinks {
		if err := sink.Append(ctx, entry); err != nil {
			return types.AuditEntry{}, fmt.Errorf("audit: sink %q append: %w", sink.Name(), err)
		}
	}
	a.prev = newHead
	return entry, nil
}

// Query routes to the named sink (or the first sink when sinkName
// is empty). The two sinks share the same entries but may diverge
// if one is compromised — querying both and comparing is the
// operator's detection path.
func (a *AuditLog) Query(ctx context.Context, sinkName string, filter types.AuditFilter) ([]types.AuditEntry, error) {
	sink, err := a.pickSink(sinkName)
	if err != nil {
		return nil, err
	}
	return sink.Query(ctx, filter)
}

// VerifyChain runs the named sink's chain walk (or every sink when
// empty). Returns per-sink results so the caller can surface
// "raft clean, local broken at ID X" to the operator.
func (a *AuditLog) VerifyChain(ctx context.Context, sinkName string) (map[string]VerifyResult, error) {
	out := make(map[string]VerifyResult, len(a.sinks))
	if sinkName != "" {
		sink, err := a.pickSink(sinkName)
		if err != nil {
			return nil, err
		}
		r, err := sink.VerifyChain(ctx)
		if err != nil {
			return nil, err
		}
		out[sink.Name()] = r
		return out, nil
	}
	for _, sink := range a.sinks {
		r, err := sink.VerifyChain(ctx)
		if err != nil {
			return nil, fmt.Errorf("audit: verify %q: %w", sink.Name(), err)
		}
		out[sink.Name()] = r
	}
	return out, nil
}

// Close shuts down every sink that implements io.Closer. Best-
// effort — a close failure on one sink is logged but doesn't
// prevent closing the rest.
func (a *AuditLog) Close() error {
	var firstErr error
	for _, sink := range a.sinks {
		if closer, ok := sink.(interface{ Close() error }); ok {
			if err := closer.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Sinks returns the registered sinks (for introspection in tests +
// CLI listing). Caller must not mutate.
func (a *AuditLog) Sinks() []AuditSink {
	out := make([]AuditSink, len(a.sinks))
	copy(out, a.sinks)
	return out
}

// pickSink resolves a name to a sink. Empty name → first sink;
// non-empty → by Name() match.
func (a *AuditLog) pickSink(name string) (AuditSink, error) {
	if len(a.sinks) == 0 {
		return nil, errors.New("audit: no sinks configured")
	}
	if name == "" {
		return a.sinks[0], nil
	}
	for _, sink := range a.sinks {
		if sink.Name() == name {
			return sink, nil
		}
	}
	return nil, fmt.Errorf("audit: unknown sink %q", name)
}

// lastEntry asks a sink for its most recent entry. Sinks don't
// expose a "head" method, so we query with Limit=1 after sorting
// reverse-chronologically via a large Since filter. Simpler
// approach: query everything and take the last. Fine for startup
// cost on realistic audit volumes; a dedicated HeadEntry method is
// a follow-up if the scan cost ever shows up.
func lastEntry(ctx context.Context, sink AuditSink) (*types.AuditEntry, error) {
	entries, err := sink.Query(ctx, types.AuditFilter{})
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	return &entries[len(entries)-1], nil
}
