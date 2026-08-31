package main

import (
	"maps"
	"slices"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

// Who hears that something is waiting for them, when nobody said.
//
// Two things use this: the self-taught review queue, and dream
// challenges. Neither should need a second, separately-populated
// block to be heard — a queue nobody is told about is auto mode with
// extra steps, and a contradiction nobody is asked about is dream
// talking to itself.
//
// Derived rather than defaulted to a constant, because the answer is
// already in the config: the channels are the ones the gateway
// carries, and the subjects are the people trusted to approve things.

// noticeAudience is the resolved channels and subjects for review
// notices, and whether they are on at all.
type noticeAudience struct {
	Enabled  bool
	Channels []string
	Subjects []string
}

// resolveNoticeAudience works out who to nudge.
//
// Explicit values always win — an operator who named a channel or a
// subject meant that list, and quietly widening it would send a
// notice somewhere they had decided against.
//
// NOT gated on self-learning mode any more. It used to return an
// empty audience unless mode was "propose", which was right when the
// review queue was the only thing with something to say. Dream
// challenges exist wherever memory does, so that gate meant switching
// self-learning to auto silently disabled every question about
// memories that disagree — an unrelated switch deciding whether
// anybody hears about their own memory.
func resolveNoticeAudience(notify config.NotifyConfig, channels []config.GatewayChannelConfig) noticeAudience {
	if notify.Disabled {
		return noticeAudience{}
	}

	out := noticeAudience{
		Enabled:  true,
		Channels: notify.Channels,
		Subjects: notify.Subjects,
	}
	if len(out.Channels) == 0 {
		out.Channels = channelKinds(channels)
	}
	if len(out.Subjects) == 0 {
		out.Subjects = ownerSubjects(channels)
	}
	// Somebody to tell and somewhere to tell them, or it is off —
	// and saying so beats constructing a notifier that can never
	// fire.
	if len(out.Channels) == 0 || len(out.Subjects) == 0 {
		return noticeAudience{}
	}
	return out
}

// notifyConfigFor picks the block in force.
//
// The top-level [notify] wins; [self_learning.notify] is read when it
// is empty, so a config written before the move keeps working rather
// than silently losing its audience.
func notifyConfigFor(cfg *config.Config) config.NotifyConfig {
	top := cfg.Notify
	if top.Disabled || len(top.Channels) > 0 || len(top.Subjects) > 0 || top.Interval > 0 {
		return top
	}
	return cfg.SelfLearning.Notify
}

// channelKinds lists the gateway channel types in play, deduplicated.
func channelKinds(channels []config.GatewayChannelConfig) []string {
	seen := map[string]struct{}{}
	for _, ch := range channels {
		if ch.Type == "" {
			continue
		}
		seen[ch.Type] = struct{}{}
	}
	return slices.Sorted(maps.Keys(seen))
}

// ownerSubjects collects the users scoped as owner.
//
// Owner only. A public or unknown-scoped user cannot approve an
// artefact, so telling them one is waiting is noise about a door they
// cannot open.
//
// Both the bare id and the tg- prefixed form, because a Telegram turn
// is attributed under the prefixed principal while the scope map is
// keyed by the bare numeric id — the same mismatch that made a
// confirmation unattributable once already.
func ownerSubjects(channels []config.GatewayChannelConfig) []string {
	seen := map[string]struct{}{}
	for _, ch := range channels {
		for id, scope := range ch.UserScopes {
			if scope != "owner" || id == "" {
				continue
			}
			// The "user:" namespace is what the channels actually
			// pass — grantSubject and noticeSubject both build it.
			// Emitting a bare id here produced a subject list that
			// could not match anything, and the nudge reported itself
			// enabled while being unable to fire.
			seen["user:"+id] = struct{}{}
			if ch.Type == "telegram" {
				// The channel-prefixed form, which is what a Telegram
				// turn is attributed as when the account has no
				// username. When it HAS one the principal is
				// "tg-@name" instead — unpredictable from config,
				// which is why Notices.Append also matches the
				// numeric identity the channel passes alongside it.
				seen["user:tg-"+id] = struct{}{}
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}
