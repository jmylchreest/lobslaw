// Package turn carries who a turn came from and where it arrived.
//
// Its own package because it is a FACT ABOUT A CALLER, not any one
// subsystem's concern.
//
// A leaf: pkg/types, internal/identity, and the standard library.
// Nothing here may import a subsystem — everything that authorises or
// attributes anything imports this, so a single subsystem dependency
// here becomes a cycle for all of them.
package turn

import (
	"context"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Identity is who a turn came from and where it arrived — the
// facts an authorisation or attribution decision needs.
//
// It travels on the context, and deliberately not in the tool-argument
// map. That map is built from the model's own JSON output, so a value
// read out of it is a value the model can choose. Tool arguments are a
// request; identity is a fact about the caller, and the two must not
// share a channel.
//
// This used to be done by injecting synthetic "__user_id" / "__chat_id"
// keys into the args map and trusting them. It did not hold: the
// injections were conditional on the request carrying each field, so on
// a turn with no channel origin — a scheduled task, a webhook, a
// research worker — the model's own value survived. What read those
// keys was not decoration: notify chose whose devices to ring,
// commitment chose whose chat a reminder fired into, and oauth_start
// stamped who initiated a credential flow into the audit log. The
// "__scope" key was worse still: nothing ever injected it, so the scope
// prefix on that audit field could only ever have come from the model.
//
// Scrubbing the map before injecting would have closed those instances.
// It would not have closed the class, because it leaves trusted and
// untrusted values sharing one namespace, separated by a naming
// convention that the next contributor has no way to discover. A
// context value cannot be reached from inside the model's output at
// all, which makes the guarantee structural rather than procedural.
type Identity struct {
	// UserID is the caller as this channel names them — "tg-@alice", a
	// REST subject. Kept for audit and display, where what the user
	// actually arrived as is what matters. Empty for an anonymous turn.
	UserID string

	// Principal is UserID resolved to a cluster-wide identity through
	// the operator's alias map, and is what ownership and visibility
	// decisions are made against. The distinction is the point: the
	// same person arrives under a different UserID on every channel,
	// so authorising on UserID alone makes one human several — and
	// they stop finding their own history the moment they switch app.
	Principal identity.Principal

	// Scope is the caller's permission tier (Claims.Scope), not an
	// ownership or namespace marker. Recorded alongside UserID where
	// attribution wants both, as the OAuth audit trail does.
	Scope string

	// Roles are the policy subjects the caller holds — Claims.Roles,
	// which arrive either from a token's `roles` claim or from the
	// operator's [[user]] declaration for channels that have no token.
	//
	// Carried here so a builtin can put the turn back through the
	// policy engine without reaching for the request's Claims, which
	// it does not have. Holding a role decides nothing by itself: it
	// is an input to a rule, and the rule is what allows or denies.
	Roles []string

	// TurnID identifies this turn. Carried so a builtin can bound a
	// per-turn budget — the pinned-memory tools cap consecutive
	// failures so a fragile edit cannot loop the turn to exhaustion
	// and suppress the user's reply, and "this turn" has to mean
	// something for that to work.
	TurnID string

	// Channel and ChannelID address the conversation this turn is
	// happening in — "telegram" and a chat id, say. Both empty for
	// turns with no channel origin: the scheduler, commitment fires,
	// research workers.
	Channel   string
	ChannelID string

	// Shared marks a conversation MORE THAN ONE PERSON CAN READ — a
	// Slack channel or group DM, a Telegram group. False for a 1:1 DM
	// and for turns with no channel origin at all.
	//
	// The channel sets it, because only the channel knows: the same
	// (Channel, ChannelID) shape addresses both a private Slack DM and
	// a 200-person channel, and nothing downstream can tell them apart.
	//
	// It changes what passive recall may surface. In a DM, ownership
	// answers the question on its own. In a shared conversation it does
	// not, because the speaker changes between turns — see
	// memory.ForConversation.
	Shared bool

	// Timezone is the caller's IANA zone, used to render times as the
	// user would read them. Lower stakes than the rest, same problem:
	// a model that picks its own zone moves when a schedule appears to
	// fire.
	Timezone string
}

// SessionKey is the conversation this turn is in, as the session store
// addresses it. Zero when the turn has no channel origin.
func (t Identity) SessionKey() SessionKey {
	return SessionKey{Channel: t.Channel, ChannelID: t.ChannelID}
}

// AttributedTo renders the caller for an audit field, keeping the
// "scope:user" shape the OAuth tracker documents. Empty when there is
// no caller to name — better than a bare separator implying one.
func (t Identity) AttributedTo() string {
	switch {
	case t.Scope != "" && t.UserID != "":
		return t.Scope + ":" + t.UserID
	case t.UserID != "":
		return t.UserID
	default:
		return ""
	}
}

// Claims rebuilds the subject a policy rule matches against. It is a
// projection, not the original token: the turn kept only the fields
// an authorisation decision reads, and expiry was checked once at the
// door.
func (t Identity) Claims() *types.Claims {
	return &types.Claims{
		UserID: t.UserID,
		Scope:  t.Scope,
		Roles:  t.Roles,
	}
}

type identityKey struct{}

// WithIdentity attaches a turn's identity for builtins to find.
// Agent.runLoop calls this once per turn; any other driver of the
// builtins that knows its caller must do the same, and one that does
// not should attach nothing rather than guess.
func WithIdentity(ctx context.Context, t Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, t)
}

// IdentityFrom returns the turn's identity. ok is false when
// nothing attached one — an operator CLI or a test driving a builtin
// directly. Callers decide what absence means for them; there is no
// single right answer, so this does not invent one.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	t, ok := ctx.Value(identityKey{}).(Identity)
	return t, ok
}

// SessionKey identifies one conversation, as the session store
// addresses it.
//
// The gateway and memory packages each carry their own copy to avoid
// an import cycle. This one is a leaf, so those two now have somewhere
// to converge rather than a reason to stay separate.
type SessionKey struct {
	Channel   string
	ChannelID string
}
