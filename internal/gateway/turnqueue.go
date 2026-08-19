package gateway

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// A turn is Load history → run the agent (seconds, with tool loops) →
// Append. Nothing made that atomic, and in webhook mode every update
// arrives on its own net/http goroutine. Two messages during one turn
// both read the same prior history and both append, so the transcript
// interleaves and duplicates — and now that sessions are durable, the
// corruption is durable too rather than evaporating with the cache.
//
// Polling mode never had this: dispatchUpdate is called in a plain
// loop, so updates were already serialised there. The gate makes the
// two paths agree instead of leaving correctness dependent on which
// transport an operator chose.
//
// It also answers the ordinary human habit of sending three short
// messages in a row, which previously raced with itself.

// QueueMode decides what happens to a message that arrives while a
// turn is already running for the same session.
type QueueMode string

const (
	// QueueSerial queues behind the in-flight turn and runs in
	// arrival order. The safe default: nothing is dropped, nothing
	// interleaves.
	QueueSerial QueueMode = "serial"

	// QueueLatest keeps only the newest queued message and discards
	// the ones it overtook. For deployments where a stale question is
	// worse than a missing one.
	QueueLatest QueueMode = "latest"

	// QueueDebounce holds briefly and folds consecutive messages into
	// a single turn. Matches how people actually type — three
	// fragments become one turn with one reply, rather than three
	// turns racing to answer half a thought each.
	QueueDebounce QueueMode = "debounce"

	// QueueOff drops messages that arrive mid-turn. The caller is
	// expected to tell the user something is still running.
	QueueOff QueueMode = "off"

	// QueueSmart is debounce with the decision made by reading the
	// messages instead of watching a clock.
	//
	// "what about rain? wind?" then "sunset? sunrise?" is one question
	// typed in two bursts. "what about rain? wind?" then "also cancel
	// the 3pm meeting" is two, and debounce folds both because four
	// seconds cannot tell them apart — losing the second in the
	// direction that matters.
	//
	// Falls back to folding when the judge is absent, errors or times
	// out. Debounce is the behaviour this mode is a refinement of, so
	// a classification outage costs precision, never a message.
	QueueSmart QueueMode = "smart"
)

// DefaultDebounce is the fold window when debounce mode is on and the
// operator has not chosen one.
const DefaultDebounce = 3 * time.Second

// ParseQueueMode maps config text to a mode, defaulting to serial.
// Serial rather than the operator's likely intent, because the modes
// that drop messages should never be reached by a typo.
func ParseQueueMode(s string) QueueMode {
	switch QueueMode(strings.ToLower(strings.TrimSpace(s))) {
	case QueueLatest:
		return QueueLatest
	case QueueSmart:
		return QueueSmart
	case QueueDebounce:
		return QueueDebounce
	case QueueOff:
		return QueueOff
	default:
		return QueueSerial
	}
}

// IsValid reports whether s names a real mode. Config validation uses
// this so a typo fails at boot rather than silently becoming serial.
func (m QueueMode) IsValid() bool {
	switch m {
	case QueueSerial, QueueLatest, QueueDebounce, QueueOff, QueueSmart:
		return true
	}
	return false
}

// Disposition is what the gate decided about one inbound message.
type Disposition int

const (
	// Admitted means this caller owns the session and must run the
	// turn, then call Lease.Release.
	Admitted Disposition = iota

	// Folded means the message was merged into another caller's turn.
	// This caller must NOT run a turn and must not reply — the turn
	// that absorbed it answers for both.
	Folded

	// Dropped means the message was discarded: QueueOff during a
	// turn, or overtaken under QueueLatest. The caller should tell
	// the user, since nothing else will.
	Dropped
)

// Lease is ownership of a session for the duration of one turn.
type Lease struct {
	gate *TurnGate
	key  string

	// cluster is the raft-backed lease, nil when no leaser is wired.
	cluster LeaseHandle
	// log is captured at mint. The heartbeat goroutine must not read
	// l.gate: Release nils it, and that is a write racing the read.
	log *slog.Logger
	// stopBeat ends the heartbeat goroutine on release.
	stopBeat chan struct{}
	beatOnce sync.Once

	// Batch is every message this turn is answering: the one that was
	// admitted, plus any folded into it while it waited. Callers must
	// use this rather than the message they arrived with, or a folded
	// fragment is silently ignored.
	Batch []string

	// Superseded counts messages dropped under QueueLatest to let
	// this turn run. Callers may mention it; the count exists so the
	// discard is at least observable.
	Superseded int
}

// Release hands the session to the next waiter. Safe to call once;
// further calls are no-ops so a deferred Release after an early
// return cannot double-release.
func (l *Lease) Release() {
	if l == nil || l.gate == nil {
		return
	}
	g := l.gate
	l.gate = nil

	// Stop heartbeating before releasing, or a beat in flight can
	// re-extend a lease we have just given up.
	if l.stopBeat != nil {
		l.beatOnce.Do(func() { close(l.stopBeat) })
	}
	if l.cluster != nil {
		// Deliberately not the turn's context: that is usually dead by
		// the time a turn finishes, and releasing is what lets the
		// user's next message start immediately instead of waiting out
		// the lease TTL.
		l.cluster.Release(context.WithoutCancel(context.Background()))
	}
	g.release(l.key)
}

// TurnGate serialises turns per session.
//
// Two layers, because they answer different questions. The in-process
// queue decides what happens to a message that arrives mid-turn on
// THIS node — fold it, drop it, or wait — and is where the queue modes
// live. The cluster lease, when a leaser is wired, decides whether
// this node may run the conversation at all. A single node needs only
// the first; two gateways serving one conversation need both.
type TurnGate struct {
	mode     QueueMode
	debounce time.Duration
	log      *slog.Logger

	// leaser is the cluster half. Nil leaves the gate in-process
	// only, which is right for a single node and is also what every
	// existing test gets.
	leaser SessionLeaser
	// heartbeat is how often a held lease is extended. Must be well
	// inside the lease TTL, or a long turn loses its lease to a peer
	// that reasonably concluded this node had died.
	heartbeat time.Duration

	// judge decides whether an arriving message continues the one
	// being collected. Only consulted in QueueSmart; nil there falls
	// back to folding, which is debounce.
	judge RelatednessJudge

	mu       sync.Mutex
	sessions map[string]*gateState
}

// WithJudge attaches the relatedness judge QueueSmart consults.
//
// Optional and chainable like WithLeaser, so a gate without one still
// works — as debounce, which is the mode smart refines.
func (g *TurnGate) WithJudge(j RelatednessJudge) *TurnGate {
	g.judge = j
	return g
}

type gateState struct {
	running bool
	// waiters is the FIFO of turns blocked on this session. Serial
	// keeps them all; latest keeps at most one; debounce folds into
	// the head rather than appending.
	waiters []*waiter
}

type waiter struct {
	ready      chan Disposition
	turnID     string
	batch      []string
	superseded int
}

// WithLeaser attaches the cluster half. Returns the gate for chaining
// at construction; not safe to call once turns are running.
func (g *TurnGate) WithLeaser(l SessionLeaser, heartbeat time.Duration) *TurnGate {
	g.leaser = l
	if heartbeat > 0 {
		g.heartbeat = heartbeat
	}
	return g
}

// NewTurnGate builds a gate. A zero debounce with QueueDebounce takes
// DefaultDebounce; debounce is ignored in every other mode. The gate
// is in-process until WithLeaser adds the cluster half.
func NewTurnGate(mode QueueMode, debounce time.Duration, log *slog.Logger) *TurnGate {
	if !mode.IsValid() {
		mode = QueueSerial
	}
	if (mode == QueueDebounce || mode == QueueSmart) && debounce <= 0 {
		debounce = DefaultDebounce
	}
	if log == nil {
		log = slog.Default()
	}
	return &TurnGate{
		mode:      mode,
		debounce:  debounce,
		log:       log,
		heartbeat: DefaultHeartbeat,
		sessions:  make(map[string]*gateState),
	}
}

// DefaultHeartbeat is a third of the default lease TTL, so two beats
// can be lost before a peer concludes this node is gone.
const DefaultHeartbeat = 30 * time.Second

// Acquire decides what happens to one inbound message and, when the
// answer is Admitted, blocks until this caller owns the session.
//
// text is the message body, carried so that debounce and latest can
// fold or replace it; the returned Lease.Batch is what the turn must
// actually answer.
//
// A cancelled ctx while queued yields Dropped: the user's client has
// gone, and running a turn to answer nobody costs tokens and may
// still write to the transcript.
func (g *TurnGate) Acquire(ctx context.Context, key, turnID, text string) (*Lease, Disposition) {
	g.mu.Lock()

	st := g.sessions[key]
	if st == nil {
		st = &gateState{}
		g.sessions[key] = st
	}

	if !st.running {
		st.running = true
		g.mu.Unlock()
		// Debounce applies to an idle session too, or the first
		// fragment of a burst always starts its own turn and only the
		// rest fold — which is the case the mode exists to prevent.
		if g.mode == QueueDebounce || g.mode == QueueSmart {
			return g.foldWindow(ctx, key, turnID, text), Admitted
		}
		return g.mint(ctx, key, turnID, []string{text}, 0)
	}

	switch g.mode {
	case QueueOff:
		g.mu.Unlock()
		return nil, Dropped

	case QueueDebounce:
		// Fold into whoever is already waiting; only start a new
		// waiter if nobody is.
		if len(st.waiters) > 0 {
			w := st.waiters[0]
			w.batch = append(w.batch, text)
			g.mu.Unlock()
			return nil, Folded
		}

	case QueueSmart:
		// Same shape as debounce, with the decision read from the
		// messages. The judge runs OUTSIDE the lock: it makes a
		// network call, and holding the gate across one would stall
		// every other conversation on this node.
		if len(st.waiters) > 0 {
			pending := append([]string(nil), st.waiters[0].batch...)
			g.mu.Unlock()
			if g.foldsWith(ctx, pending, text) {
				g.mu.Lock()
				// Re-checked after the unlock: the waiter may have
				// been admitted or dropped while the judge thought.
				// Folding into a batch that has already run would
				// answer nobody.
				if st := g.sessions[key]; st != nil && len(st.waiters) > 0 {
					st.waiters[0].batch = append(st.waiters[0].batch, text)
					g.mu.Unlock()
					return nil, Folded
				}
				g.mu.Unlock()
			}
			// Unrelated, or the fold raced: queue it as its own turn.
			// NEVER dropped — hermes-agent's adapter takes the same
			// position, appending to queued_prompts when a redirect
			// does not land rather than discarding the message.
			g.mu.Lock()
			st = g.sessions[key]
			if st == nil {
				st = &gateState{running: true}
				g.sessions[key] = st
			}
			w := &waiter{ready: make(chan Disposition, 1), turnID: turnID, batch: []string{text}}
			st.waiters = append(st.waiters, w)
			g.mu.Unlock()
			return g.wait(ctx, key, w)
		}

	case QueueLatest:
		// Overtake anyone queued. They have not run, so discarding
		// them here is the whole point of the mode.
		for _, w := range st.waiters {
			w.ready <- Dropped
		}
		dropped := len(st.waiters)
		st.waiters = nil
		w := &waiter{ready: make(chan Disposition, 1), turnID: turnID, batch: []string{text}, superseded: dropped}
		st.waiters = append(st.waiters, w)
		g.mu.Unlock()
		return g.wait(ctx, key, w)
	}

	w := &waiter{ready: make(chan Disposition, 1), turnID: turnID, batch: []string{text}}
	st.waiters = append(st.waiters, w)
	g.mu.Unlock()
	return g.wait(ctx, key, w)
}

// wait blocks until this waiter is handed the session or gives up.
func (g *TurnGate) wait(ctx context.Context, key string, w *waiter) (*Lease, Disposition) {
	select {
	case d := <-w.ready:
		if d != Admitted {
			return nil, d
		}
		// Handed the session, but the caller may have given up while
		// we were being handed it. Returning Admitted here would be
		// correct only if every caller released even on a context it
		// had already abandoned — and the obvious way to write such a
		// caller ("ctx dead? return") leaks the session forever.
		// Admitted therefore means the context was still live at
		// hand-off.
		if err := ctx.Err(); err != nil {
			g.release(key)
			return nil, Dropped
		}
		return g.mint(ctx, key, w.turnID, w.batch, w.superseded)

	case <-ctx.Done():
		g.mu.Lock()
		found := false
		if st := g.sessions[key]; st != nil {
			for i, other := range st.waiters {
				if other == w {
					st.waiters = append(st.waiters[:i], st.waiters[i+1:]...)
					found = true
					break
				}
			}
		}
		g.mu.Unlock()
		if found {
			return nil, Dropped
		}

		// We were not in the queue, so something already took us out
		// of it and sent a verdict — release(), or a latest-mode
		// eviction, or a fold window. The send is buffered, so it
		// completed whether or not we were reading.
		//
		// If that verdict was Admitted we now own a session we are
		// about to abandon, and dropping it on the floor wedges the
		// conversation for good. Hand it on.
		if d := <-w.ready; d == Admitted {
			g.release(key)
		}
		return nil, Dropped
	}
}

// foldWindow holds an idle-session turn open for the debounce window
// so fragments typed straight after the first join the same turn.
func (g *TurnGate) foldWindow(ctx context.Context, key, turnID, text string) *Lease {
	timer := time.NewTimer(g.debounce)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		// Still return a lease: we hold the session and must release
		// it. The caller sees a cancelled ctx and can abandon.
	}

	// Absorb what queued during the window.
	//
	// debounce takes everything; smart takes what belongs. Whatever
	// smart leaves stays queued and runs as its own turn, so a change
	// of subject typed during the window is answered separately
	// instead of being appended to an unrelated question.
	g.mu.Lock()
	batch := []string{text}
	var keep []*waiter
	st := g.sessions[key]
	if st != nil {
		waiters := st.waiters
		st.waiters = nil
		g.mu.Unlock()

		// Judged outside the lock — each call is a round trip, and
		// the gate is shared by every conversation on this node.
		for _, w := range waiters {
			joined := strings.Join(w.batch, "\n")
			if g.mode == QueueSmart && !g.foldsWith(ctx, batch, joined) {
				keep = append(keep, w)
				continue
			}
			batch = append(batch, w.batch...)
			w.ready <- Folded
		}

		g.mu.Lock()
		if st = g.sessions[key]; st != nil {
			// Prepended: anything that arrived while the judge was
			// thinking queued behind these, and answering in arrival
			// order is what serial promises and smart does not
			// contradict.
			st.waiters = append(keep, st.waiters...)
		}
	}
	g.mu.Unlock()
	l, _ := g.mint(ctx, key, turnID, batch, 0)
	return l
}

// release hands the session to the next waiter, or marks it idle.
func (g *TurnGate) release(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	st := g.sessions[key]
	if st == nil {
		return
	}
	if len(st.waiters) == 0 {
		// Drop the entry so a long-lived process does not accumulate
		// one map slot per conversation it has ever seen.
		delete(g.sessions, key)
		return
	}

	next := st.waiters[0]
	st.waiters = st.waiters[1:]

	// Under debounce, everything still queued belongs to this turn:
	// they arrived while it was blocked, which is exactly the burst
	// the mode folds.
	if g.mode == QueueDebounce {
		for _, w := range st.waiters {
			next.batch = append(next.batch, w.batch...)
			w.ready <- Folded
		}
		st.waiters = nil
	}
	next.ready <- Admitted
}

// Mode reports the configured queue mode, so callers can tailor what
// they tell the user about a dropped message.
func (g *TurnGate) Mode() QueueMode { return g.mode }

// The gate above serialises turns inside one process. A conversation
// can be reached from more than one at a time — a webhook and a REST
// client, or two gateways behind a load balancer — and those have
// different maps. SessionLeaser is the part that holds across nodes.

// ErrLeaseUnavailable means the cluster lease for this conversation
// could not be taken. The adapter translates the memory package's
// sentinel onto this so the gateway need not import it.
var ErrLeaseUnavailable = errors.New("gateway: session lease unavailable")

// SessionLeaser takes cluster-wide ownership of a conversation for the
// duration of a turn. Nil disables the cluster half, which is correct
// for a single-node deployment: there is no second process to race.
type SessionLeaser interface {
	// AcquireLease returns a handle, or an error wrapping
	// ErrLeaseUnavailable when another node holds the conversation.
	AcquireLease(ctx context.Context, key, turnID string) (LeaseHandle, error)
}

// LeaseHandle is a held cluster lease. Implementations must tolerate a
// nil receiver so a caller that has no leaser needs no branch.
type LeaseHandle interface {
	// Heartbeat extends the lease. Returning an error means the lease
	// was lost and the turn should stop.
	Heartbeat(ctx context.Context) error
	Release(ctx context.Context)
}

// mint builds the lease handed to a caller, taking the cluster lease
// on the way. One chokepoint deliberately: a path that returned a
// Lease without it would serialise locally and silently not across
// nodes, which looks identical until two gateways are running.
//
// Failing to take the cluster lease means another node owns this
// conversation, so the local slot is given back and the caller is told
// to drop. The queue mode already decided we were willing to run —
// this is the cluster disagreeing.
func (g *TurnGate) mint(ctx context.Context, key, turnID string, batch []string, superseded int) (*Lease, Disposition) {
	l := &Lease{gate: g, key: key, log: g.log, Batch: batch, Superseded: superseded}
	if g.leaser == nil {
		return l, Admitted
	}

	handle, err := g.leaser.AcquireLease(ctx, key, turnID)
	if err != nil {
		g.log.Info("gateway: conversation is held by another node",
			"key", key, "err", err)
		g.release(key)
		return nil, Dropped
	}
	l.cluster = handle
	l.stopBeat = make(chan struct{})
	go l.beat(g.heartbeat)
	return l, Admitted
}

// beat extends the cluster lease until release. A turn that outruns
// the TTL without this is treated as dead by peers, and the
// conversation is taken over while it is still running.
func (l *Lease) beat(every time.Duration) {
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-l.stopBeat:
			return
		case <-t.C:
			// Background context: the turn's context may already be
			// cancelled by a hard timeout while the agent is still
			// unwinding, and dropping the lease at that moment would
			// hand the conversation to a peer mid-write.
			if err := l.cluster.Heartbeat(context.WithoutCancel(context.Background())); err != nil {
				l.log.Warn("gateway: lost the cluster lease mid-turn",
					"key", l.key, "err", err)
				return
			}
		}
	}
}
