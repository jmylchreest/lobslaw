package gateway

import (
	"context"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// responsivenessDefaults apply when a TelegramConfig leaves a timer
// field at zero. Defaults chosen for Telegram chat-app pace: typing
// refreshes inside the 5s Telegram clears at; interim "still
// working" at 30s which is long enough that silence feels wrong;
// hard cap at 90s which bounds worst-case latency under 2min.
const (
	defaultTypingInterval = 4 * time.Second
	defaultInterimTimeout = 30 * time.Second
	defaultHardTimeout    = 90 * time.Second

	// directnessChattyCutoff is the EmotiveStyle.Directness score
	// below which interim messages are emitted. 7+ = direct
	// personality — skip filler. Lower = chatty — send updates.
	directnessChattyCutoff = 7
)

// startResponsivenessGuards spins up the three concurrent timers
// for a turn. Returns:
//   - turnCtx: child context that cancels on hard-timeout; callers
//     pass this to the agent so a stalled LLM call aborts
//   - cleanup: call after the agent returns to stop all timers
//
// Zero values on any config field disable that specific timer.
// Nil Soul → interim messages are universal (can't check directness).
func (h *TelegramHandler) startResponsivenessGuards(ctx context.Context, chatID int64) (context.Context, func()) {
	typingEvery := h.cfg.TypingInterval
	if typingEvery <= 0 {
		typingEvery = defaultTypingInterval
	}
	interimAfter := h.cfg.InterimTimeout
	if interimAfter <= 0 {
		interimAfter = defaultInterimTimeout
	}
	hardAfter := h.cfg.HardTimeout
	if hardAfter <= 0 {
		hardAfter = defaultHardTimeout
	}

	turnCtx, cancel := context.WithTimeout(ctx, hardAfter)
	done := make(chan struct{})

	if typingEvery > 0 {
		go h.typingKeepalive(turnCtx, chatID, typingEvery, done)
	}

	if interimAfter > 0 && h.shouldEmitInterim() {
		go h.interimNotifier(turnCtx, chatID, interimAfter, done)
	}

	cleanup := func() {
		close(done)
		cancel()
	}
	return turnCtx, cleanup
}

// shouldEmitInterim consults the current SOUL's directness score
// to decide whether the bot is chatty enough to send "still
// working" updates. High directness = terse bot = skip filler.
func (h *TelegramHandler) shouldEmitInterim() bool {
	if h.cfg.Soul == nil {
		return true
	}
	soul := h.cfg.Soul()
	if soul == nil {
		return true
	}
	return soul.EmotiveStyle.Directness < directnessChattyCutoff
}

// typingKeepalive POSTs sendChatAction(typing) every interval until
// done closes. Telegram shows "typing..." for ~5s then clears, so
// the interval must be under 5s to maintain a continuous indicator.
func (h *TelegramHandler) typingKeepalive(ctx context.Context, chatID int64, interval time.Duration, done <-chan struct{}) {
	// Fire one immediately so the user sees "typing" within milliseconds.
	h.sendChatAction(ctx, chatID, "typing")
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			h.sendChatAction(ctx, chatID, "typing")
		}
	}
}

// interimNotifier waits for the threshold, then sends a "still
// working on this" message if the turn hasn't completed yet.
// Single-shot — doesn't spam further messages after the first one.
func (h *TelegramHandler) interimNotifier(ctx context.Context, chatID int64, after time.Duration, done <-chan struct{}) {
	select {
	case <-done:
		return
	case <-ctx.Done():
		return
	case <-time.After(after):
		h.sendText(chatID, "Still working on this — a few tools are running…")
	}
}

// sendChatAction POSTs to Telegram's sendChatAction endpoint. Cheap
// API — no reply message, just a presence signal. Errors are logged
// and swallowed (loss of a typing indicator is a soft failure).
func (h *TelegramHandler) sendChatAction(_ context.Context, chatID int64, action string) {
	// Inline rather than building a full request struct — payload
	// is just {chat_id, action}.
	body := map[string]any{
		"chat_id": chatID,
		"action":  action,
	}
	h.postJSON("sendChatAction", body)
}

// ensure type import side-effect (types.SoulConfig may be unused in
// package otherwise).
var _ = types.SoulConfig{}
