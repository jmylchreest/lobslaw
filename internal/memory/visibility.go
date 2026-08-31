package memory

import (
	"github.com/jmylchreest/lobslaw/internal/identity"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Audience is who a read is on behalf of. Every search takes one.
//
// It exists as a type rather than a string because the string version
// already failed. Search used to take a `scopeFilter string` where ""
// meant "everything", and both production call sites passed "" — the
// agent's memory_search tool and, worse, the ContextEngine, which
// injects recalled memories into the system prompt on every turn with
// no tool call in front of it. On a shared node that put one user's
// memories into another's prompt before they had said anything.
//
// The lesson was not that two callers were careless. It was that the
// dangerous value was the easy one to write: "" is what you get from a
// zero value, an unset variable, or not thinking about it. So here the
// zero Audience matches nothing, and seeing everything has to be
// spelled Everyone() — three call sites want it and each is a caller
// that already holds the whole database.
type Audience struct {
	// set distinguishes the zero value from a deliberate anonymous
	// audience. Without it, Audience{} and For("") are the same, and
	// the accident is indistinguishable from the intent.
	set bool
	// everyone bypasses the filter entirely.
	everyone bool
	// principal is the canonical identity the read is for.
	principal identity.Principal
	// conversation, when non-empty, widens the audience to records
	// this conversation produced, as "<channel>:<channel_id>".
	//
	// Only set for a conversation SEVERAL PEOPLE CAN READ. In a DM,
	// ownership already says everything: the one person reading owns
	// what they should see. In a Slack channel it does not — the
	// speaker changes between turns, and recall keyed on the speaker
	// alone would surface whatever the last person to type happens to
	// own, to an audience that never owned any of it.
	//
	// So a shared conversation reads: what the speaker owns, plus what
	// this conversation itself produced. The second half is what keeps
	// the agent useful in a team channel rather than amnesiac.
	conversation string
}

// For returns the audience for a principal. A zero principal — an
// anonymous turn — still produces a set Audience: anonymous means
// "owns nothing", not "sees nothing", so such a caller still reads
// shared and legacy records.
func For(p identity.Principal) Audience {
	return Audience{set: true, principal: p}
}

// ForConversation returns the audience for a principal speaking in a
// conversation others can read, addressed as "<channel>:<channel_id>".
//
// An empty conversation degrades to For(p) rather than widening: the
// dangerous direction here is the one that costs nothing to write, and
// a caller that could not name its conversation has not thereby earned
// a view of every conversation.
func ForConversation(p identity.Principal, conversation string) Audience {
	return Audience{set: true, principal: p, conversation: conversation}
}

// Everyone is the unrestricted read, spelled out so it can be grepped
// and reviewed. Legitimate for Dream consolidation, the operator's
// `lobslaw memory` CLI, and compaction — callers that hold the whole
// store already and gain nothing from being refused a view of it.
func Everyone() Audience {
	return Audience{set: true, everyone: true}
}

// IsZero reports an Audience nobody set. Callers that can refuse
// should; the search paths treat it as matching nothing.
func (a Audience) IsZero() bool { return !a.set }

// allows decides one record.
//
// Three ways in, in order of how often they matter:
//
//   - Shared. Operator-seeded knowledge and anything about the
//     deployment rather than about a person.
//   - Owned. The principal matches.
//
// An UNOWNED record is readable by nobody but Everyone(). There used to
// be a carve-out making it readable by all, on the grounds that an
// upgrade must not hide an existing node's whole memory — but lobslaw
// has never been deployed, so there are no records predating ownership
// and that carve-out was a standing fail-open guarding the empty set.
// Nothing writes an empty owner either: every Claims construction in
// the tree yields one ("anon" for unauthenticated REST,
// "webhook:<name>", "scheduler", the Telegram identity), so an unowned
// record now means a bug upstream, and being invisible is how it
// surfaces rather than how it hides.
//
// One deliberate exception: the dream-session audit line, which
// describes a run rather than recording anything anyone said. See
// logDreamSession.
//   - Same conversation. Only when the audience names one, which only
//     happens for a conversation with an audience — see ForConversation.
//     A record with no origin ("" session_ref) matches no conversation,
//     so a scheduled task's memory never arrives this way.
func (a Audience) allows(owner string, vis lobslawv1.Visibility, sessionRef string) bool {
	if !a.set {
		return false
	}
	if a.everyone {
		return true
	}
	if vis == lobslawv1.Visibility_VISIBILITY_SHARED {
		return true
	}
	if !a.principal.IsZero() && owner == a.principal.String() {
		return true
	}
	return a.conversation != "" && sessionRef == a.conversation
}

// AllowsVector reports whether a vector record is readable. Exported
// for callers that filter a result set they already hold.
func (a Audience) AllowsVector(rec *lobslawv1.VectorRecord) bool {
	if rec == nil {
		return false
	}
	return a.allows(rec.Owner, rec.Visibility, rec.SessionRef)
}

// AllowsEpisodic reports whether an episodic record is readable.
func (a Audience) AllowsEpisodic(rec *lobslawv1.EpisodicRecord) bool {
	if rec == nil {
		return false
	}
	return a.allows(rec.Owner, rec.Visibility, rec.SessionRef)
}
