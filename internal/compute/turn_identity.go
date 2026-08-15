package compute

import "context"

// TurnIdentity is who a turn came from and where it arrived — the
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
type TurnIdentity struct {
	// UserID is the caller — Claims.UserID. Note this is per-channel
	// ("tg-@alice", a REST subject), not a cluster-wide person: two
	// channels used by the same human are two identities. Empty for a
	// genuinely anonymous turn.
	UserID string

	// Scope is the caller's permission tier (Claims.Scope), not an
	// ownership or namespace marker. Recorded alongside UserID where
	// attribution wants both, as the OAuth audit trail does.
	Scope string

	// Channel and ChannelID address the conversation this turn is
	// happening in — "telegram" and a chat id, say. Both empty for
	// turns with no channel origin: the scheduler, commitment fires,
	// research workers.
	Channel   string
	ChannelID string

	// Timezone is the caller's IANA zone, used to render times as the
	// user would read them. Lower stakes than the rest, same problem:
	// a model that picks its own zone moves when a schedule appears to
	// fire.
	Timezone string
}

// SessionKey is the conversation this turn is in, as the session store
// addresses it. Zero when the turn has no channel origin.
func (t TurnIdentity) SessionKey() SessionKey {
	return SessionKey{Channel: t.Channel, ChannelID: t.ChannelID}
}

// AttributedTo renders the caller for an audit field, keeping the
// "scope:user" shape the OAuth tracker documents. Empty when there is
// no caller to name — better than a bare separator implying one.
func (t TurnIdentity) AttributedTo() string {
	switch {
	case t.Scope != "" && t.UserID != "":
		return t.Scope + ":" + t.UserID
	case t.UserID != "":
		return t.UserID
	default:
		return ""
	}
}

type turnIdentityKey struct{}

// WithTurnIdentity attaches a turn's identity for builtins to find.
// Agent.runLoop calls this once per turn; any other driver of the
// builtins that knows its caller must do the same, and one that does
// not should attach nothing rather than guess.
func WithTurnIdentity(ctx context.Context, t TurnIdentity) context.Context {
	return context.WithValue(ctx, turnIdentityKey{}, t)
}

// TurnIdentityFrom returns the turn's identity. ok is false when
// nothing attached one — an operator CLI or a test driving a builtin
// directly. Callers decide what absence means for them; there is no
// single right answer, so this does not invent one.
func TurnIdentityFrom(ctx context.Context) (TurnIdentity, bool) {
	t, ok := ctx.Value(turnIdentityKey{}).(TurnIdentity)
	return t, ok
}
