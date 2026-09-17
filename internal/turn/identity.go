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
	//
	// Channel may also be SYNTHETIC — see ChannelBot. A synthetic
	// channel names a transcript, not a place a person is waiting, and
	// the difference matters to anything that tries to reply on it.
	Channel   string
	ChannelID string

	// RequestedBy is the person this work traces back to, when that is
	// not the caller of this turn.
	//
	// It exists because delegation loses the requester. "Prepare a
	// report and send it to James" reaches the coordinator as Sam,
	// becomes a queue item for research, and by the time research
	// sends anything the turn is running as bot:research — Sam is
	// gone, and James receives a message with no idea who asked for
	// it. Carried through the hop so provenance survives the thing
	// that destroys it.
	//
	// Empty for work nobody asked for: a routine firing at 3am has no
	// requester, and inventing one would be worse than having none.
	RequestedBy string

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

	// BotID names the bot taking this turn, empty for the node's
	// default assistant.
	//
	// Carried separately from Principal even though Principal is
	// derived from it, because "which bot is this" and "who owns what
	// this turn writes" are different questions with different answers
	// on a turn a bot runs FOR somebody: a routine alice scheduled is
	// worked by the devops bot and attributed to alice.
	BotID string
}

// IsBot reports whether a bot is taking this turn rather than the
// node's default assistant. Read by the places that must attribute a
// bot's work to it — notification sender labels, inter-bot messaging.
func (t Identity) IsBot() bool { return t.BotID != "" }

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

// ChannelBot is the synthetic channel a bot's own working transcripts
// are filed under.
//
// Synthetic because there is nobody on the other end of it: it exists
// so an inbox turn's conversation can be stored and linked to, not so
// anything can be delivered there. Nothing registers a sink for it and
// nothing should.
//
// Named here rather than in the gateway because the distinction is
// about identity, and the code that most needs it — notify, deciding
// whether to reply on the originating channel or broadcast — cannot
// import the gateway.
const ChannelBot = "bot"

// IsHumanChannel reports whether this turn arrived somewhere a person
// could be waiting for a reply.
//
// False for a synthetic channel and for no channel at all. The two
// cases behave identically on purpose: a 3am routine and an inbox item
// both have an audience of nobody, and before bot transcripts were
// persisted they were indistinguishable because both had an empty
// Channel. Giving inbox turns a channel name is what made this
// necessary — it silently turned "notify whoever asked to be told"
// into "reply into a transcript", and notify started failing with
// "no sink registered for channel \"bot\"".
func (t Identity) IsHumanChannel() bool {
	return t.Channel != "" && t.Channel != ChannelBot
}
