package main

import (
	"sort"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

// Who hears about a review queue, when nobody said.
//
// mode = "propose" is already the statement that a human should look
// before anything the agent wrote takes effect. Making the nudge a
// second, separately-populated block meant propose mode defaulted to
// writing into a queue nobody was told about — auto mode with extra
// steps, and worse, because proposal expiry then discards things
// nobody declined.
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
func resolveNoticeAudience(selfLearningMode string, notify config.NotifyConfig, channels []config.GatewayChannelConfig) noticeAudience {
	// Only propose mode has a queue. Auto applies artefacts
	// immediately and off writes none, so there is nothing waiting on
	// a person in either.
	if selfLearningMode != "propose" || notify.Disabled {
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
	// A queue with nobody to tell is still off, and saying so beats
	// constructing a notifier that can never fire.
	if len(out.Channels) == 0 || len(out.Subjects) == 0 {
		return noticeAudience{}
	}
	return out
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
	return sortedKeys(seen)
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
	return sortedKeys(seen)
}

func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
