package compute

import (
	"context"
	"sync"
)

// A watch check has to get an answer out of a turn, and a turn's
// return value is prose written for a person.
//
// The probe reports through a tool call rather than in its reply, and
// the tool hands the value back here — the same shape as
// WithArtifactCollector and WithCostCollector, for the same reason:
// a builtin has no reference to whoever started the turn, and giving
// it one would let a tool reach back into the caller's decisions.

type watchCollectorKey struct{}

// WatchReport is one probe's answer.
type WatchReport struct {
	// State is the canonical fact the watch compares between checks.
	// Digested, never shown: two checks that report the same state are
	// the same observation however differently they were phrased
	// around it.
	State string
	// Summary is what the user is told when State has changed. Prose,
	// and deliberately not part of the comparison — a reworded summary
	// of an unchanged fact is not a change.
	Summary string
}

// WatchCollector receives the report a probe turn makes.
type WatchCollector struct {
	mu     sync.Mutex
	report *WatchReport
	calls  int
}

// WithWatchCollector installs a collector for one probe turn.
func WithWatchCollector(ctx context.Context) (context.Context, *WatchCollector) {
	c := &WatchCollector{}
	return context.WithValue(ctx, watchCollectorKey{}, c), c
}

// ReportWatch records a probe's answer, reporting whether there was
// anywhere to record it.
//
// False means this turn is not a watch check. Unlike CollectArtifact,
// which no-ops silently because any turn may legitimately produce a
// file, a watch report outside a watch check is a mistake worth
// telling the model about: nothing downstream will ever read it.
func ReportWatch(ctx context.Context, r WatchReport) bool {
	c, ok := ctx.Value(watchCollectorKey{}).(*WatchCollector)
	if !ok || c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	// Last write wins. A probe that reports twice is confused rather
	// than malicious, and the later value is the one it settled on;
	// Calls records that it happened so the handler can log it.
	rep := r
	c.report = &rep
	return true
}

// Report returns what the probe reported, and how many times it
// reported. A nil report with zero calls is a silent probe, which the
// handler treats as a failed check rather than as an unchanged one.
func (c *WatchCollector) Report() (*WatchReport, int) {
	if c == nil {
		return nil, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.report, c.calls
}
