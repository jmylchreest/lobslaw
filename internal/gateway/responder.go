package gateway

import (
	"context"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// The interaction quality in the Telegram handler — a typing indicator
// refreshed inside Telegram's 5s clear window, an interim "still
// working" at 30s, a hard cap at 90s, all gated on whether the SOUL is
// chatty enough to warrant filler — was better than anything either
// reference project documented, and it was Telegram-only.
//
// REST got none of it, and every future channel would either
// reimplement it or, more likely, not. So the timers move behind an
// interface and are written once.

// Responder is how a channel speaks back during a turn.
//
// Deliberately three methods, not four. R10 proposed a Prompt method
// too, and confirmation rendering is genuinely different in shape per
// channel — Telegram sends an inline keyboard and waits for a callback,
// REST returns a prompt id and long-polls. Both already work. An
// interface over two things that differ that much, for a third channel
// that does not exist yet, would be a guess rather than a
// generalisation; it can be added when a channel arrives that makes
// the right shape obvious.
type Responder interface {
	// Typing signals that work is happening. A no-op where the channel
	// has no such concept — a webhook has nobody watching.
	Typing(ctx context.Context) error

	// Interim delivers a progress message mid-turn. Called at most
	// once per turn.
	Interim(ctx context.Context, text string) error

	// Final delivers the turn's reply.
	Final(ctx context.Context, text string) error
}

// responsivenessDefaults apply when a config leaves a timer at zero.
// Chosen for chat-app pace: typing refreshes inside the 5s window
// Telegram clears at; interim at 30s, which is long enough that
// silence starts to feel wrong; a hard cap at 90s bounding worst-case
// latency under two minutes.
const (
	defaultTypingInterval = 4 * time.Second
	defaultInterimTimeout = 30 * time.Second

	// DefaultHardTimeout is exported because the REST server's write
	// deadline is derived from it: a socket that closes before the
	// turn's own cap can never deliver the forced-summary reply.
	DefaultHardTimeout = 90 * time.Second
	defaultHardTimeout = DefaultHardTimeout

	// directnessChattyCutoff is the EmotiveStyle.Directness score at
	// or above which interim messages are skipped. A terse personality
	// emitting "still working on this…" reads as a different assistant
	// than the one the operator configured.
	directnessChattyCutoff = 7
)

// ResponsivenessConfig tunes the turn timers. Zero on any field takes
// the default; a negative value disables that timer, which is how a
// channel with no live viewer opts out of typing without opting out of
// the hard timeout.
type ResponsivenessConfig struct {
	TypingInterval time.Duration
	InterimTimeout time.Duration
	HardTimeout    time.Duration

	// InterimText is what a slow turn says. Empty takes a default.
	InterimText string

	// Soul, when set, gates interim messages on the personality's
	// directness. Nil emits them universally — a channel with no SOUL
	// wired should not silently lose progress reporting.
	Soul func() *types.SoulConfig
}

// defaultInterimText is deliberately specific about *why* it is slow.
// "Still working" alone reads as a stall; naming tools tells the user
// the turn is doing something rather than stuck.
const defaultInterimText = "Still working on this — a few tools are running…"

// startResponsiveness spins up the turn timers against any channel.
//
// Returns a child context that cancels at the hard timeout — callers
// pass it to the agent so a stalled LLM call aborts rather than
// hanging a conversation — and a cleanup func to stop the timers once
// the turn returns.
//
// Errors from the responder are ignored on purpose. Losing a typing
// indicator is a soft failure, and a turn that aborted because a
// presence ping 500'd would be worse than one that looks briefly
// silent.
func startResponsiveness(ctx context.Context, r Responder, cfg ResponsivenessConfig) (context.Context, func()) {
	typingEvery := orDefault(cfg.TypingInterval, defaultTypingInterval)
	interimAfter := orDefault(cfg.InterimTimeout, defaultInterimTimeout)
	hardAfter := orDefault(cfg.HardTimeout, defaultHardTimeout)

	turnCtx := ctx
	cancel := func() {}
	if hardAfter > 0 {
		turnCtx, cancel = context.WithTimeout(ctx, hardAfter)
	}
	done := make(chan struct{})

	if r != nil && typingEvery > 0 {
		go typingKeepalive(turnCtx, r, typingEvery, done)
	}
	if r != nil && interimAfter > 0 && shouldEmitInterim(cfg.Soul) {
		text := cfg.InterimText
		if text == "" {
			text = defaultInterimText
		}
		go interimNotifier(turnCtx, r, interimAfter, text, done)
	}

	var stopped bool
	return turnCtx, func() {
		// Guarded because a caller that defers cleanup AND calls it on
		// an early return would otherwise close a closed channel and
		// take the process down over a tidiness mistake.
		if stopped {
			return
		}
		stopped = true
		close(done)
		cancel()
	}
}

// orDefault resolves a timer: zero takes the default, negative
// disables, positive is used as given.
func orDefault(v, def time.Duration) time.Duration {
	if v == 0 {
		return def
	}
	if v < 0 {
		return 0
	}
	return v
}

// shouldEmitInterim asks the SOUL whether this assistant is chatty
// enough to narrate its own delays.
func shouldEmitInterim(soul func() *types.SoulConfig) bool {
	if soul == nil {
		return true
	}
	s := soul()
	if s == nil {
		return true
	}
	return s.EmotiveStyle.Directness < directnessChattyCutoff
}

// typingKeepalive re-signals presence until the turn ends. Telegram
// clears "typing…" after about five seconds, so the interval has to
// stay under that to read as continuous; other channels are free to
// make Typing a no-op.
func typingKeepalive(ctx context.Context, r Responder, interval time.Duration, done <-chan struct{}) {
	// Immediately, so the user sees presence within milliseconds
	// rather than after one interval of apparent silence.
	_ = r.Typing(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			_ = r.Typing(ctx)
		}
	}
}

// interimNotifier sends one progress message if the turn is still
// running at the threshold. Single-shot: a turn that is slow is not
// improved by being told about it repeatedly.
func interimNotifier(ctx context.Context, r Responder, after time.Duration, text string, done <-chan struct{}) {
	t := time.NewTimer(after)
	defer t.Stop()
	select {
	case <-done:
	case <-ctx.Done():
	case <-t.C:
		_ = r.Interim(ctx, text)
	}
}
