package gateway

import (
	"context"

	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

// scriptedJudge answers NEW for any incoming containing "cancel",
// SAME otherwise — enough to tell the two cases apart without a
// model.
type scriptedJudge struct {
	mu    sync.Mutex
	calls int
	err   error
	block time.Duration
}

func (j *scriptedJudge) Related(ctx context.Context, _ []string, incoming string) (bool, error) {
	j.mu.Lock()
	j.calls++
	j.mu.Unlock()
	if j.block > 0 {
		select {
		case <-time.After(j.block):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	if j.err != nil {
		return false, j.err
	}
	return !strings.Contains(strings.ToLower(incoming), "cancel"), nil
}

func smartGate(t *testing.T, j RelatednessJudge) *TurnGate {
	t.Helper()
	return NewTurnGate(QueueSmart, 150*time.Millisecond,
		slog.New(slog.DiscardHandler)).WithJudge(j)
}

// mustReceive reads a disposition or fails.
//
// Bounded because the interesting failure is a message that QUEUES
// when it should have folded — and a queued waiter blocks until the
// running turn releases, which a failing test has not reached yet. A
// bare receive turns that into a ten-minute hang instead of a
// sentence naming the mode.
func mustReceive(t *testing.T, ch <-chan Disposition, what string) Disposition {
	t.Helper()
	select {
	case d := <-ch:
		return d
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: nothing came back — the message queued when it should have folded", what)
		return Dropped
	}
}

// send fires an Acquire in the background and reports what it got.
func send(g *TurnGate, key, turnID, text string) <-chan Disposition {
	out := make(chan Disposition, 1)
	go func() {
		lease, d := g.Acquire(context.Background(), key, turnID, text)
		if lease != nil {
			lease.Release()
		}
		out <- d
	}()
	return out
}

// "what's the weather?" then "sunset? sunrise?" is one question typed
// in two bursts. Debounce gets this right by accident; smart must get
// it right on purpose.
func TestAFollowUpIsFolded(t *testing.T) {
	t.Parallel()
	j := &scriptedJudge{}
	g := smartGate(t, j)

	first := make(chan *Lease, 1)
	go func() {
		l, _ := g.Acquire(context.Background(), "s1", "t1", "what's the weather in York?")
		first <- l
	}()
	time.Sleep(20 * time.Millisecond)
	second := send(g, "s1", "t2", "sunset? sunrise?")

	if d := mustReceive(t, second, "follow-up"); d != Folded {
		t.Errorf("a follow-up got %v, want Folded", d)
	}
	l := <-first
	if l == nil {
		t.Fatal("no lease")
	}
	defer l.Release()
	if len(l.Batch) != 2 {
		t.Errorf("batch = %v, want both messages in one turn", l.Batch)
	}
}

// THE CASE DEBOUNCE GETS WRONG. A different request that happens to
// arrive inside the window must be answered separately, not appended
// to an unrelated question.
func TestAChangeOfSubjectIsNotFolded(t *testing.T) {
	t.Parallel()
	j := &scriptedJudge{}
	g := smartGate(t, j)

	first := make(chan *Lease, 1)
	go func() {
		l, _ := g.Acquire(context.Background(), "s2", "t1", "what's the weather in York?")
		first <- l
	}()
	time.Sleep(20 * time.Millisecond)
	second := send(g, "s2", "t2", "also cancel the 3pm meeting")

	l := <-first
	if l == nil {
		t.Fatal("no lease")
	}
	if len(l.Batch) != 1 {
		t.Errorf("batch = %v; the unrelated message was folded in", l.Batch)
	}
	// Releasing hands the session to the message that was kept back.
	l.Release()
	if d := mustReceive(t, second, "change of subject"); d != Admitted {
		t.Errorf("the unrelated message got %v, want Admitted — it must run as its own turn", d)
	}
	if j.calls == 0 {
		t.Error("the judge was never consulted")
	}
}

// AS SPECIFIED: smart requires a working preflight, and without one
// it must behave as debounce. A classification outage costs
// precision, never a message.
func TestAJudgeErrorFoldsLikeDebounce(t *testing.T) {
	t.Parallel()
	g := smartGate(t, &scriptedJudge{err: errors.New("provider down")})

	first := make(chan *Lease, 1)
	go func() {
		l, _ := g.Acquire(context.Background(), "s3", "t1", "what's the weather?")
		first <- l
	}()
	time.Sleep(20 * time.Millisecond)
	second := send(g, "s3", "t2", "also cancel the 3pm meeting")

	if d := mustReceive(t, second, "judge-error fallback"); d != Folded {
		t.Errorf("with the judge failing the message got %v, want Folded (debounce behaviour)", d)
	}
	l := <-first
	defer l.Release()
	if len(l.Batch) != 2 {
		t.Errorf("batch = %v, want the fallback to have folded both", l.Batch)
	}
}

// A judge that hangs must not hold the turn open. The timeout is
// shorter than the window it runs inside, and expiry folds.
func TestASlowJudgeFoldsRatherThanStalling(t *testing.T) {
	t.Parallel()
	g := smartGate(t, &scriptedJudge{block: 10 * time.Second})

	done := make(chan []string, 1)
	go func() {
		l, _ := g.Acquire(context.Background(), "s4", "t1", "what's the weather?")
		done <- l.Batch
		l.Release()
	}()
	time.Sleep(20 * time.Millisecond)
	_ = send(g, "s4", "t2", "also cancel the 3pm meeting")

	select {
	case batch := <-done:
		if len(batch) != 2 {
			t.Errorf("batch = %v, want the timeout to have folded", batch)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a hanging judge stalled the turn")
	}
}

// No judge at all is debounce, which is what smart refines. A node
// without compute must not fail on queue_mode = "smart".
func TestSmartWithoutAJudgeIsDebounce(t *testing.T) {
	t.Parallel()
	g := smartGate(t, nil)
	first := make(chan *Lease, 1)
	go func() {
		l, _ := g.Acquire(context.Background(), "s5", "t1", "one")
		first <- l
	}()
	time.Sleep(20 * time.Millisecond)
	if d := mustReceive(t, send(g, "s5", "t2", "also cancel everything"), "no judge"); d != Folded {
		t.Errorf("without a judge the message got %v, want Folded", d)
	}
	l := <-first
	defer l.Release()
	if len(l.Batch) != 2 {
		t.Errorf("batch = %v", l.Batch)
	}
}

// The mode has to survive config: a typo must not silently become it,
// and the name must round-trip.
func TestSmartParsesAndValidates(t *testing.T) {
	t.Parallel()
	if got := ParseQueueMode("smart"); got != QueueSmart {
		t.Errorf("ParseQueueMode(smart) = %v", got)
	}
	if got := ParseQueueMode("smrat"); got != QueueSerial {
		t.Errorf("a typo became %v, want serial", got)
	}
	if !QueueSmart.IsValid() {
		t.Error("smart is not valid, so config validation would reject it")
	}
}

// The mode vocabulary lives in two places by necessity — pkg/config
// sits below this package and must not import it — so the agreement
// is asserted rather than hoped for.
//
// Not hypothetical: "smart" was added here, passed every test in this
// file, and then failed at BOOT because the loader's list had not
// grown with it.
func TestConfigAcceptsEveryModeThisPackageDefines(t *testing.T) {
	t.Parallel()
	for _, m := range []QueueMode{QueueSerial, QueueLatest, QueueDebounce, QueueOff, QueueSmart} {
		if err := config.ValidateQueueModeForTest(string(m)); err != nil {
			t.Errorf("%q is a valid mode here but the config loader rejects it: %v", m, err)
		}
	}
}

// And the other direction: a name the loader accepts must resolve to
// a real mode, or config permits something that silently becomes
// serial.
func TestEveryModeConfigAcceptsIsReal(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"serial", "latest", "debounce", "off", "smart"} {
		if err := config.ValidateQueueModeForTest(name); err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if got := ParseQueueMode(name); string(got) != name {
			t.Errorf("config accepts %q but it parses to %q", name, got)
		}
	}
}

// The judge budget is the fold window, not a constant.
//
// A fixed 2s was wrong in both directions on real providers: it cut
// off a preflight model measured at 3.1s and would have been needless
// latency on one measured at 0.64s. Deriving it means widening the
// window because people type slowly also gives the judge room,
// without a second knob to discover.
func TestTheJudgeBudgetFollowsTheFoldWindow(t *testing.T) {
	t.Parallel()
	g := NewTurnGate(QueueSmart, 7*time.Second, slog.New(slog.DiscardHandler))
	if got := g.judgeTimeout(); got != 7*time.Second {
		t.Errorf("judge budget = %v, want the configured window", got)
	}
	// A zero window takes the default rather than no timeout at all,
	// which would let a hung provider hold the turn forever.
	g = NewTurnGate(QueueSmart, 0, slog.New(slog.DiscardHandler))
	if got := g.judgeTimeout(); got != DefaultDebounce {
		t.Errorf("judge budget = %v, want the default window", got)
	}
}
