package node

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/tools"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func newWatchState(base, maxInterval time.Duration) *lobslawv1.WatchState {
	return &lobslawv1.WatchState{
		Interval:     durationpb.New(base),
		BaseInterval: durationpb.New(base),
		MaxInterval:  durationpb.New(maxInterval),
		MaxFailures:  3,
	}
}

func report(state, summary string) *compute.WatchReport {
	return &compute.WatchReport{State: state, Summary: summary}
}

// The whole promise of a watch: it stays quiet while the answer is the
// same, and the gap between checks widens while it does.
func TestWatchUnchangedSaysNothingAndBacksOff(t *testing.T) {
	t.Parallel()
	st := newWatchState(time.Hour, 24*time.Hour)
	now := time.Now()

	first := decideWatchResult(st, "the fare", report("price: 212.00 GBP", ""), now)
	if first.Action != watchBaseline {
		t.Fatalf("first check should be the baseline; got %s", first.Action)
	}
	if first.Message != "" {
		t.Errorf("the baseline must be silent; got %q", first.Message)
	}

	second := decideWatchResult(st, "the fare", report("price: 212.00 GBP", ""), now.Add(time.Hour))
	if second.Action != watchUnchanged {
		t.Fatalf("action = %s, want unchanged", second.Action)
	}
	if second.Message != "" {
		t.Errorf("an unchanged check must say nothing; got %q", second.Message)
	}
	if second.Retry <= time.Hour {
		t.Errorf("interval should widen past the base; got %s", second.Retry)
	}

	third := decideWatchResult(st, "the fare", report("price: 212.00 GBP", ""), now.Add(2*time.Hour))
	if third.Retry <= second.Retry {
		t.Errorf("interval should keep widening: %s then %s", second.Retry, third.Retry)
	}
}

func TestWatchChangeSpeaksOnceAndResetsTheInterval(t *testing.T) {
	t.Parallel()
	st := newWatchState(time.Hour, 24*time.Hour)
	now := time.Now()
	decideWatchResult(st, "the fare", report("price: 212.00 GBP", ""), now)
	decideWatchResult(st, "the fare", report("price: 212.00 GBP", ""), now.Add(time.Hour))

	changed := decideWatchResult(st, "the fare",
		report("price: 189.00 GBP", "The fare dropped to £189."), now.Add(2*time.Hour))
	if changed.Action != watchChanged {
		t.Fatalf("action = %s, want changed", changed.Action)
	}
	if !strings.Contains(changed.Message, "£189") {
		t.Errorf("the summary should reach the user; got %q", changed.Message)
	}
	// Both states, because "it changed" without the old value is the
	// half of the message people actually want.
	if !strings.Contains(changed.Message, "212.00") || !strings.Contains(changed.Message, "189.00") {
		t.Errorf("message should carry both states; got %q", changed.Message)
	}
	if changed.Retry != time.Hour {
		t.Errorf("a change should reset the interval to the base; got %s", changed.Retry)
	}
	if st.UnchangedRuns != 0 {
		t.Errorf("unchanged run count should reset on a change; got %d", st.UnchangedRuns)
	}

	// The same change, observed again, is not a change.
	again := decideWatchResult(st, "the fare",
		report("price: 189.00 GBP", "The fare dropped to £189."), now.Add(3*time.Hour))
	if again.Action != watchUnchanged {
		t.Fatalf("action = %s, want unchanged", again.Action)
	}
	if again.Message != "" {
		t.Errorf("the same change must not be announced twice; got %q", again.Message)
	}
}

// Rewording is the failure this design exists to prevent: a model that
// restates an unchanged fact differently must not be read as news.
// The digest cannot fix that on its own — the prompt has to carry the
// previous state so the model has something to match.
func TestWatchProbePromptReplaysThePreviousObservation(t *testing.T) {
	t.Parallel()
	st := newWatchState(time.Hour, 24*time.Hour)
	st.Observation = "price: 212.00 GBP"

	prompt := watchProbePrompt("the fare on BA117", st)
	if !strings.Contains(prompt, "price: 212.00 GBP") {
		t.Errorf("previous observation must appear verbatim in the prompt; got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "watch_report") {
		t.Errorf("the prompt must name the tool it requires; got:\n%s", prompt)
	}

	fresh := newWatchState(time.Hour, 24*time.Hour)
	first := watchProbePrompt("the fare on BA117", fresh)
	if !strings.Contains(first, "first check") {
		t.Errorf("a baseline prompt should say so; got:\n%s", first)
	}
}

// A broken probe and a quiet one look identical from the outside, and
// treating the first as the second is how a watch dies without anyone
// noticing.
func TestWatchSilentProbeIsAFailureNotAnUnchangedResult(t *testing.T) {
	t.Parallel()
	st := newWatchState(time.Hour, 24*time.Hour)
	now := time.Now()
	decideWatchResult(st, "the fare", report("price: 212.00 GBP", ""), now)

	d := decideWatchFailure(st, "the fare", "the check ran but reported no state", now.Add(time.Hour))
	if d.Action != watchFailed {
		t.Fatalf("action = %s, want failed", d.Action)
	}
	if st.UnchangedRuns != 0 {
		t.Errorf("a failure must not count as an unchanged run; got %d", st.UnchangedRuns)
	}
	if st.FailedRuns != 1 {
		t.Errorf("failed runs = %d, want 1", st.FailedRuns)
	}
	if st.Observation != "price: 212.00 GBP" {
		t.Errorf("a failed probe must leave the last real observation alone; got %q", st.Observation)
	}
	if st.Digest == "" {
		t.Error("a failed probe must not clear the digest")
	}
}

func TestWatchSuspendsAfterMaxFailuresAndSaysWhy(t *testing.T) {
	t.Parallel()
	st := newWatchState(time.Hour, 24*time.Hour) // MaxFailures: 3
	now := time.Now()
	why := "the check could not run"

	if d := decideWatchFailure(st, "the fare", why, now); d.Message != "" || d.Retry == 0 {
		t.Fatalf("first failure should retry quietly; got %+v", d)
	}
	if d := decideWatchFailure(st, "the fare", why, now); d.Message != "" || d.Retry == 0 {
		t.Fatalf("second failure should retry quietly; got %+v", d)
	}
	third := decideWatchFailure(st, "the fare", why, now)
	if third.Action != watchSuspended {
		t.Fatalf("action = %s, want suspended", third.Action)
	}
	if third.Retry != 0 {
		t.Errorf("a suspended watch must not re-arm; got retry %s", third.Retry)
	}
	if !strings.Contains(third.Message, why) {
		t.Errorf("suspension must name the reason; got %q", third.Message)
	}
	if st.SuspendedReason != why {
		t.Errorf("suspended reason = %q, want %q", st.SuspendedReason, why)
	}
}

// A successful check after a bad patch means the probe works again.
func TestWatchSuccessClearsTheFailureRun(t *testing.T) {
	t.Parallel()
	st := newWatchState(time.Hour, 24*time.Hour)
	now := time.Now()
	decideWatchFailure(st, "the fare", "transient", now)
	decideWatchFailure(st, "the fare", "transient", now)
	if st.FailedRuns != 2 {
		t.Fatalf("failed runs = %d, want 2", st.FailedRuns)
	}
	decideWatchResult(st, "the fare", report("price: 212.00 GBP", ""), now)
	if st.FailedRuns != 0 {
		t.Errorf("a successful check should clear the failure run; got %d", st.FailedRuns)
	}
}

// Nothing in this feature ends in silence: a watch that stops without
// saying so leaves the user believing it is still running.
func TestWatchExpirySpeaksAndDoesNotRearm(t *testing.T) {
	t.Parallel()
	st := newWatchState(time.Hour, 24*time.Hour)
	now := time.Now()
	st.ExpiresAt = timestamppb.New(now.Add(-time.Minute))
	st.LastChanged = timestamppb.New(now.Add(-72 * time.Hour))

	d, expired := decideWatchExpiry(st, "the fare", now)
	if !expired {
		t.Fatal("a watch past its expiry should expire")
	}
	if d.Message == "" {
		t.Error("expiry must tell the user; a silent ending is indistinguishable from still running")
	}
	if d.Retry != 0 {
		t.Errorf("an expired watch must not re-arm; got %s", d.Retry)
	}
	if st.SuspendedReason != "expired" {
		t.Errorf("suspended reason = %q, want \"expired\"", st.SuspendedReason)
	}

	live := newWatchState(time.Hour, 24*time.Hour)
	live.ExpiresAt = timestamppb.New(now.Add(time.Hour))
	if _, expired := decideWatchExpiry(live, "the fare", now); expired {
		t.Error("a watch inside its expiry should not expire")
	}
}

func TestWatchBackoffIsCapped(t *testing.T) {
	t.Parallel()
	st := newWatchState(time.Hour, 4*time.Hour)
	st.UnchangedRuns = 50
	if got := nextWatchInterval(st); got != 4*time.Hour {
		t.Errorf("interval = %s, want the 4h cap", got)
	}

	// A max below the base is a coherent request — "do not back off" —
	// rather than an error, and must not produce a shrinking interval.
	odd := newWatchState(time.Hour, time.Minute)
	odd.UnchangedRuns = 3
	if got := nextWatchInterval(odd); got != time.Hour {
		t.Errorf("interval = %s, want the base when max < base", got)
	}
}

// The digest is the comparison, so what it does and does not fold
// together is a behavioural decision rather than an implementation
// detail.
func TestWatchDigestIgnoresSurroundingWhitespaceOnly(t *testing.T) {
	t.Parallel()
	if watchDigest("price: 212.00 GBP") != watchDigest("  price: 212.00 GBP\n") {
		t.Error("surrounding whitespace should not read as a change")
	}
	if watchDigest("status: OK") == watchDigest("status: ok") {
		t.Error("case carries meaning in the states people watch and must not be folded")
	}
	if watchDigest("price: 212.00 GBP") == watchDigest("price: 212.0 GBP") {
		t.Error("a different value must be a different digest")
	}
}

// A change the model failed to summarise still has to be reportable.
func TestWatchChangeWithoutSummaryStillCarriesBothStates(t *testing.T) {
	t.Parallel()
	st := newWatchState(time.Hour, 24*time.Hour)
	now := time.Now()
	decideWatchResult(st, "the fare", report("price: 212.00 GBP", ""), now)
	d := decideWatchResult(st, "the fare", report("price: 189.00 GBP", ""), now.Add(time.Hour))
	if d.Message == "" {
		t.Fatal("a change must produce a message even with no summary")
	}
	if !strings.Contains(d.Message, "212.00") || !strings.Contains(d.Message, "189.00") {
		t.Errorf("fallback message should carry both states; got %q", d.Message)
	}
}

// The turn that runs a check is the only one that may report, so it is
// the only one offered the tool. Everywhere else the model would see a
// tool that fails in every context but this one.
func TestWatchTurnIsOfferedTheReportingTool(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	for _, td := range tools.WatchToolDefs() {
		if err := reg.Register(td); err != nil {
			t.Fatal(err)
		}
	}

	var advertised bool
	for _, tl := range reg.LLMTools() {
		if tl.Name == "watch_report" {
			advertised = true
		}
	}
	if advertised {
		t.Error("watch_report is advertised on ordinary turns")
	}

	var offered bool
	for _, tl := range buildWatchToolList(reg) {
		if tl.Name == "watch_report" {
			offered = true
			if len(tl.Parameters) == 0 {
				t.Error("the tool was offered without its schema; the model cannot call it")
			}
		}
	}
	if !offered {
		t.Error("a watch check was not offered watch_report — the check can never report")
	}

	if got := buildWatchToolList(nil); got != nil {
		t.Errorf("a nil registry should yield no tools; got %d", len(got))
	}
}

// The handler decides whether a watch speaks. A probe that could call
// notify would be able to message the user from inside an unchanged
// check, which is the one thing the feature promises not to do — and
// a probe that could schedule would be able to schedule itself.
func TestWatchProbeCannotSpeakOrSchedule(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	for _, td := range tools.WatchToolDefs() {
		if err := reg.Register(td); err != nil {
			t.Fatal(err)
		}
	}
	// Stand-ins for the tools a real node registers alongside.
	for _, name := range []string{"notify", "commitment_create", "schedule_create", "web_search"} {
		if err := reg.Register(&types.ToolDef{
			Name:     name,
			Path:     compute.BuiltinScheme + name,
			RiskTier: types.RiskReversible,
		}); err != nil {
			t.Fatal(err)
		}
	}

	offered := map[string]bool{}
	for _, tl := range buildWatchToolList(reg) {
		offered[tl.Name] = true
	}

	for _, denied := range []string{"notify", "commitment_create", "schedule_create", "watch_create", "watch_cancel"} {
		if offered[denied] {
			t.Errorf("a probe turn was offered %q", denied)
		}
	}
	// It still has to be able to find things out, and to report.
	if !offered["web_search"] {
		t.Error("a probe cannot check anything without its read tools")
	}
	if !offered["watch_report"] {
		t.Error("a probe that cannot report can never be anything but a failed check")
	}
}
