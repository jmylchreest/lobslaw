package gateway

import "time"

// A fold window you only pay for when it buys something.
//
// The window can only combine two messages by making the first one
// wait, and measured on this cluster that is the whole cost: a lone
// message took 1.5s with no window, 2.6s at one second, and 7.4s at
// five. Lone messages are the common case, so a fixed window taxes
// the many to serve the few.
//
// So the window starts at zero — instant — and is LEARNED. A message
// arriving while a turn is running is evidence that this person types
// in bursts, and the next turn on that conversation opens a window
// for them. Stop bursting and it decays back to instant.
//
// The judge is unaffected either way: it only ever runs against
// messages that are already waiting, so an idle session with nothing
// pending has never made a classification call.

const (
	// DefaultBurstWindow is what a detected burst sets the window to.
	//
	// Matches DefaultDebounce, because it is the same judgement about
	// human typing cadence — this one is just applied to people who
	// have shown they need it rather than to everybody.
	DefaultBurstWindow = DefaultDebounce

	// DefaultBurstReset is how long a conversation must go without
	// bursting before the learned window decays to nothing.
	//
	// Five minutes: long enough to cover a pause for thought inside
	// one exchange, short enough that a conversation which has moved
	// on stops paying for a habit it no longer has.
	DefaultBurstReset = 5 * time.Minute
)

// noteBurst records that a message arrived while this session was
// busy, which is the evidence a window would have helped.
//
// Called with the gate lock held.
func (g *TurnGate) noteBurst(st *gateState, now time.Time) {
	st.burstWindow = g.burstWindow
	st.lastBurst = now
}

// windowFor is how long this session should hold the first message.
//
// The configured window is a FLOOR, not an override: an operator who
// asked for two seconds gets at least two, and a session that bursts
// can still earn more. Zero from both means start immediately, which
// is the default and the common case.
//
// Called with the gate lock held.
func (g *TurnGate) windowFor(st *gateState, now time.Time) time.Duration {
	learned := time.Duration(0)
	if st != nil && st.burstWindow > 0 {
		// Decayed on read rather than on a timer. A conversation
		// nobody is having does not need a goroutine to forget it.
		if now.Sub(st.lastBurst) < g.burstReset {
			learned = st.burstWindow
		} else {
			st.burstWindow = 0
		}
	}
	if g.debounce > learned {
		return g.debounce
	}
	return learned
}

// worthKeeping reports whether a session's state should outlive its
// last turn.
//
// Only a live burst memory is worth a map slot: everything else is
// reconstructable from the next message, and the entry is dropped so
// a long-lived process does not accumulate one per conversation it
// has ever seen.
//
// Called with the gate lock held.
func (g *TurnGate) worthKeeping(st *gateState, now time.Time) bool {
	return st != nil && st.burstWindow > 0 && now.Sub(st.lastBurst) < g.burstReset
}
