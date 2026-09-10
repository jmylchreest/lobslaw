package node

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/tools"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// What one check decides, separated from running it.
//
// Every rule that governs whether a watch speaks lives here, as
// functions over a WatchState and a report. The handler beside this
// does the I/O — run a turn, send a message, ask the scheduler for
// another go — and takes no decisions of its own, so the behaviour
// people care about is testable without a node, a provider or a
// cluster.

type watchAction int

const (
	watchBaseline watchAction = iota
	watchUnchanged
	watchChanged
	watchFailed
	watchSuspended
	watchExpired
)

func (a watchAction) String() string {
	switch a {
	case watchBaseline:
		return "baseline"
	case watchUnchanged:
		return "unchanged"
	case watchChanged:
		return "changed"
	case watchFailed:
		return "failed"
	case watchSuspended:
		return "suspended"
	case watchExpired:
		return "expired"
	default:
		return "unknown"
	}
}

// watchDecision is what the handler does next.
//
// Message empty means say nothing — the normal outcome, and the one
// that makes a watch worth having. Retry zero means the watch is over.
type watchDecision struct {
	Action  watchAction
	Message string
	Retry   time.Duration
}

// watchBackoffFactor widens the gap between checks while nothing is
// changing. 1.5 rather than 2 because doubling reaches the cap in
// four quiet checks from an hourly start, and a watch that has gone
// quiet for four hours is not evidence that the thing being watched
// has become slower to change.
const watchBackoffFactor = 1.5

// decideWatchExpiry runs before the probe. A watch that has run out
// has no reason to spend a provider call proving it.
//
// The message is not optional. A watch that ends in silence is
// indistinguishable from one that is still running and has nothing to
// say, so the user goes on believing they are covered by something
// that stopped weeks ago.
func decideWatchExpiry(st *lobslawv1.WatchState, what string, now time.Time) (watchDecision, bool) {
	if st.ExpiresAt == nil || st.ExpiresAt.AsTime().IsZero() || !now.After(st.ExpiresAt.AsTime()) {
		return watchDecision{}, false
	}
	st.SuspendedReason = "expired"
	return watchDecision{
		Action: watchExpired,
		Message: fmt.Sprintf("I've stopped watching %s — it hadn't changed since %s.",
			what, watchSince(st)),
	}, true
}

// decideWatchResult folds one report into the state.
func decideWatchResult(st *lobslawv1.WatchState, what string, report *compute.WatchReport, now time.Time) watchDecision {
	digest := watchDigest(report.State)
	st.LastChecked = timestamppb.New(now)
	// A check that produced an observation clears the failure run: the
	// probe demonstrably works again, and carrying old failures would
	// suspend a healthy watch on the strength of a bad afternoon.
	st.FailedRuns = 0

	switch st.Digest {
	case "":
		// Baseline, and deliberately silent. The user asked to be told
		// when something changes; the first look is what it changes
		// from.
		st.Digest = digest
		st.Observation = report.State
		st.LastChanged = timestamppb.New(now)
		st.UnchangedRuns = 0
		st.Interval = durationpb.New(nextWatchInterval(st))
		return watchDecision{Action: watchBaseline, Retry: st.Interval.AsDuration()}

	case digest:
		st.UnchangedRuns++
		st.Interval = durationpb.New(nextWatchInterval(st))
		return watchDecision{Action: watchUnchanged, Retry: st.Interval.AsDuration()}

	default:
		previous := st.Observation
		st.Digest = digest
		st.Observation = report.State
		st.LastChanged = timestamppb.New(now)
		// Reset, so a thing that starts moving is watched closely
		// again rather than at the cadence its quiet spell earned.
		st.UnchangedRuns = 0
		st.Interval = durationpb.New(nextWatchInterval(st))
		return watchDecision{
			Action:  watchChanged,
			Message: watchChangeMessage(what, previous, report),
			Retry:   st.Interval.AsDuration(),
		}
	}
}

// decideWatchFailure counts a probe that produced no observation.
//
// A failure is not an unchanged result, and the distinction is the
// point of the whole function. Counting a broken probe as "nothing has
// changed" leaves a dead watch looking exactly like a quiet one —
// the worst outcome available, because it is invisible.
//
// The observation is deliberately untouched: whatever was last
// genuinely seen stays the thing the next successful probe is
// compared against.
func decideWatchFailure(st *lobslawv1.WatchState, what, why string, now time.Time) watchDecision {
	st.LastChecked = timestamppb.New(now)
	st.FailedRuns++

	limit := st.MaxFailures
	if limit == 0 {
		limit = tools.DefaultWatchMaxFailures
	}
	if st.FailedRuns >= limit {
		st.SuspendedReason = why
		return watchDecision{
			Action: watchSuspended,
			Message: fmt.Sprintf(
				"I've stopped watching %s: %s, %d times in a row. Ask me to watch it again if you'd like me to keep trying.",
				what, why, st.FailedRuns),
		}
	}
	st.Interval = durationpb.New(nextWatchInterval(st))
	return watchDecision{Action: watchFailed, Retry: st.Interval.AsDuration()}
}

// nextWatchInterval widens the gap while nothing is happening and
// resets to the base on a change.
//
// Failures widen it on the same curve: whatever is wrong is unlikely
// to be fixed by asking again immediately, and a watch that retries
// hard against a broken probe is how a quiet feature becomes an
// expensive one.
func nextWatchInterval(st *lobslawv1.WatchState) time.Duration {
	base := st.BaseInterval.AsDuration()
	if base <= 0 {
		base = tools.DefaultWatchInterval
	}
	maxInterval := st.MaxInterval.AsDuration()
	if maxInterval < base {
		maxInterval = base
	}
	out := float64(base)
	for range st.UnchangedRuns + st.FailedRuns {
		out *= watchBackoffFactor
		if out >= float64(maxInterval) {
			return maxInterval
		}
	}
	return time.Duration(out)
}

// watchProbePrompt builds the instruction one check runs.
//
// The previous state is replayed verbatim and the shape rule is stated
// next to it. That anchoring is the whole reason the digest works: the
// comparison is character-for-character, so a model that reports "212
// pounds" having reported "price: 212.00 GBP" an hour earlier has
// announced a change that did not happen.
func watchProbePrompt(what string, st *lobslawv1.WatchState) string {
	var b strings.Builder
	b.WriteString("You are running a scheduled watch check. Nobody is waiting on this reply — ")
	b.WriteString("the only thing that leaves this turn is the watch_report call, so make it.\n\n")
	b.WriteString("What to check: ")
	b.WriteString(what)
	b.WriteString("\n\n")
	if st.Observation != "" {
		b.WriteString("The state you reported last time was:\n\n    ")
		b.WriteString(st.Observation)
		b.WriteString("\n\nReport this time's state in exactly that shape — same fields, same units, ")
		b.WriteString("same formatting — so that an unchanged fact produces an identical string. ")
		b.WriteString("If the thing you are watching has genuinely changed, the value changes and the shape does not.\n\n")
	} else {
		b.WriteString("This is the first check, so you are establishing the baseline. ")
		b.WriteString("Choose the shortest canonical form of the fact — a label and a value, no prose — ")
		b.WriteString("and remember that every later check will be asked to match it exactly.\n\n")
	}
	b.WriteString("Use whatever tools you need to find out, then call watch_report once. ")
	b.WriteString("Call it even when nothing has changed: silence is read as a broken check, not as a quiet one.")
	return b.String()
}

func watchChangeMessage(what, previous string, report *compute.WatchReport) string {
	summary := strings.TrimSpace(report.Summary)
	if summary == "" {
		// A change with no summary still has to be reportable, and the
		// states are what actually changed.
		summary = fmt.Sprintf("%s changed.", what)
	}
	return fmt.Sprintf("%s\n\nWas: %s\nNow: %s", summary, previous, report.State)
}

func watchSince(st *lobslawv1.WatchState) string {
	if st.LastChanged == nil || st.LastChanged.AsTime().IsZero() {
		return "I started watching"
	}
	return st.LastChanged.AsTime().Format("2 Jan")
}

func watchDigest(state string) string {
	// Trimmed, because trailing whitespace from a model is noise
	// rather than news. Nothing else is normalised: case and
	// punctuation carry meaning in the states people watch, and
	// folding them here would silently swallow real changes.
	sum := sha256.Sum256([]byte(strings.TrimSpace(state)))
	return hex.EncodeToString(sum[:])
}
