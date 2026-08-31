package node

import (
	"time"

	"github.com/jmylchreest/lobslaw/internal/gateway"
)

// wireNotices assembles the in-channel nudge.
//
// Its own stage because it now has more than one thing to say and
// they come from different places: the self-taught review queue,
// which exists only when self-learning is on, and unresolved
// contradictions, which exist whenever memory does. Built inside the
// self-learning stage, the second was reachable only by operators who
// had enabled the first — an unrelated switch deciding whether
// anybody hears about their memories disagreeing.
//
// Runs after both stores and before the gateways that read it.
func (n *Node) wireNotices() error {
	var review gateway.NoticeSource
	if n.selfTaught != nil {
		review = pendingReviewSource{store: n.selfTaught}
	}
	var nightmares gateway.NoticeSource
	if n.store != nil {
		nightmares = &nightmareSource{
			store:   n.store,
			tz:      n.resolveUserTimezone,
			askedAt: map[string]time.Time{},
		}
	}

	src := gateway.CombineNoticeSources(review, nightmares)
	n.notices = gateway.NewNotices(src, gateway.NoticeConfig{
		Channels: n.cfg.NotifyChannels,
		Subjects: n.cfg.NotifySubjects,
		Interval: n.cfg.NotifyInterval,
	})
	if src == nil {
		return nil
	}
	if len(n.cfg.NotifyChannels) > 0 && len(n.cfg.NotifySubjects) > 0 {
		n.log.Info("notices enabled",
			"channels", n.cfg.NotifyChannels, "subjects", len(n.cfg.NotifySubjects))
		return nil
	}
	// Said out loud, because "I never got told" is otherwise
	// indistinguishable from "there was nothing to tell" — and with
	// proposal expiry running and contradictions accumulating, the
	// difference matters.
	n.log.Info("notices are off",
		"reason", "notify needs both channels and subjects")
	return nil
}
