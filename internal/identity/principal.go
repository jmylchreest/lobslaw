// Package identity resolves the per-channel user ids lobslaw receives
// into the cluster-wide principals that ownership and visibility are
// decided against.
//
// The two are not the same thing, and conflating them is what makes
// scoping either leaky or useless. A channel id is whatever that
// channel happens to call someone — "tg-@alice" from Telegram, a JWT
// subject over REST, a webhook's configured scope. The same human
// arrives under a different one on every channel, so scoping directly
// on the channel id makes one person several: they stop finding their
// own history the moment they switch app. Scoping on nothing at all
// makes everyone one person, which is the bug this exists to prevent.
//
// So the operator declares the mapping, and everything downstream
// decides against the result.
package identity

import (
	"context"
	"sort"
	"strings"
)

// Principal is a canonical, cluster-wide identity: the thing a record
// can be owned by and a request can be authorised as.
//
// Rendered as "<kind>:<id>" so the kind is legible wherever a
// principal is stored or logged, and so two kinds can never collide on
// a bare id. Three kinds exist today:
//
//	user:alice                 a person
//	chat:telegram:-1001234     a conversation, owned collectively
//	bot:engineering            one of the assistant's own agents
//
// The second matters because ownership is not always individual. A
// Telegram group chat's transcript belongs to the chat, not to whoever
// happened to speak first, and modelling that as a principal is what
// keeps group sharing from being a special case in every reader.
//
// The third is what makes a team of bots cost almost nothing: memory
// ownership, policy subjects, scheduled-task owners and audit entries
// all already decide against a principal, so a bot that IS one
// inherits per-bot isolation from machinery that already exists.
// Modelling a bot's authority as a field on a bot record instead would
// have been a second authorisation system beside the policy engine,
// and two of those is how they come to disagree.
type Principal string

// Kind prefixes. Exported because callers construct principals for
// records they are about to write.
const (
	KindUser = "user"
	KindChat = "chat"
	KindBot  = "bot"
)

// RoleOperator is the [[user]] role that names a human operator.
// Holding it grants nothing on its own; it only makes the principal
// matchable by a policy rule, and uniquely identifiable when
// adopting unowned bot records.
const RoleOperator = "operator"

// User returns the principal for a person. An empty id yields the
// empty Principal rather than "user:", so callers can test for absence
// without knowing the encoding.
func User(id string) Principal {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	return Principal(KindUser + ":" + id)
}

// Chat returns the principal for a conversation that belongs to its
// participants collectively rather than to any one of them.
func Chat(channel, channelID string) Principal {
	channel, channelID = strings.TrimSpace(channel), strings.TrimSpace(channelID)
	if channel == "" || channelID == "" {
		return ""
	}
	return Principal(KindChat + ":" + channel + ":" + channelID)
}

// Bot returns the principal for one of the assistant's own agents.
//
// Deliberately not reachable from the alias map: aliases translate the
// ids that arrive from a channel, and a channel must never be able to
// name a caller a bot. A bot principal is minted here, by the code
// that already knows it is running a bot's turn.
func Bot(id string) Principal {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	return Principal(KindBot + ":" + id)
}

// IsBot reports whether this principal names a bot rather than a
// person or a conversation.
func (p Principal) IsBot() bool {
	return strings.HasPrefix(string(p), KindBot+":")
}

// UniqueOperator returns the user:<id> principal when exactly one
// operator id is supplied. Zero or more than one yields the empty
// Principal — unowned records stay inaccessible rather than becoming
// public.
func UniqueOperator(operatorIDs []string) Principal {
	if len(operatorIDs) != 1 {
		return ""
	}
	return User(operatorIDs[0])
}

// String renders the principal for storage and logs.
func (p Principal) String() string { return string(p) }

// ID returns the identifier without its kind prefix — "alice" from
// "user:alice".
//
// For the places that need a bare id rather than a principal
// reference: types.Claims.UserID is one, because policy subjects are
// written "user:alice" and the engine adds the kind itself. Passing a
// principal there would produce "user:user:alice" and match nothing.
func (p Principal) ID() string {
	s := string(p)
	if _, rest, ok := strings.Cut(s, ":"); ok {
		return rest
	}
	return s
}

// IsZero reports the absence of an identity — an anonymous turn, or a
// record written before ownership existed.
func (p Principal) IsZero() bool { return p == "" }

// Resolver maps the per-channel user ids that arrive on a turn to
// canonical principals.
//
// Unmapped ids are NOT rejected. A deployment that never configures an
// alias still gets correct behaviour — every channel id becomes its
// own principal, which is exactly today's semantics — and only a
// deployment that wants two channel ids treated as one person needs to
// say so. Requiring registration up front would mean a new user cannot
// talk to the bot until an operator edits a file.
type Resolver struct {
	aliases map[string]Principal
	// bindings resolves a channel ADDRESS to a canonical principal.
	//
	// The alias map above keys on the id a channel already derived —
	// for Telegram that is the @username when the user has one, which
	// is reassignable, so a rename orphans every binding and whoever
	// claims the freed handle inherits them. Resolving from the raw
	// address instead lets an operator bind the numeric id, which
	// never changes.
	//
	// Optional: a deployment that configures no bindings keeps
	// today's behaviour exactly.
	bindings ChannelBindings
}

// ChannelBindings is the state-backed half of resolution: the
// operator's declared "this address is this person".
//
// An interface so this package does not depend on the memory store —
// identity is consulted from the gateway edge, and a cycle there would
// be a real problem rather than a stylistic one.
type ChannelBindings interface {
	// PrincipalFor returns the canonical user id bound to a channel
	// address, or "" when nothing is bound. An error means the lookup
	// itself failed, which is different from "not bound" and must not
	// be treated as one.
	PrincipalFor(ctx context.Context, channel, address string) (string, error)
}

// WithBindings attaches a state-backed lookup. Returns the resolver so
// it can be chained onto NewResolver at a wiring site.
func (r *Resolver) WithBindings(b ChannelBindings) *Resolver {
	if r == nil {
		return nil
	}
	r.bindings = b
	return r
}

// NewResolver builds a resolver from an operator's alias map: channel
// user id → canonical principal id.
//
//	[identity.aliases]
//	"tg-@alice"         = "alice"
//	"alice@example.com" = "alice"
//
// Values are bare ids, not "user:alice" — the kind is this package's
// business, and letting a config file choose it would let a typo mint
// a principal kind nothing else understands. Keys are matched
// case-insensitively: channels differ on whether they preserve the
// case of a handle, and "TG-@Alice" being a different person from
// "tg-@alice" is never what an operator meant.
func NewResolver(aliases map[string]string) *Resolver {
	r := &Resolver{aliases: make(map[string]Principal, len(aliases))}
	for from, to := range aliases {
		from, to = strings.TrimSpace(from), strings.TrimSpace(to)
		if from == "" || to == "" {
			continue
		}
		r.aliases[strings.ToLower(from)] = User(to)
	}
	return r
}

// Resolve returns the canonical principal for a channel user id.
// Unmapped ids become their own principal. An empty id yields the zero
// Principal — an anonymous turn owns nothing and can read nothing
// owned by anyone else.
//
// A nil Resolver resolves everything to itself, so a caller that has
// no configuration is not forced to construct one.
func (r *Resolver) Resolve(userID string) Principal {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ""
	}
	if r != nil {
		if p, ok := r.aliases[strings.ToLower(userID)]; ok {
			return p
		}
	}
	return User(userID)
}

// ResolveChannel returns the canonical principal for a channel
// address, falling back to the alias map and finally to fallbackID —
// the id the channel would have used on its own.
//
// This is the entry point a gateway should use, because it is the only
// one that sees the raw address. Resolving later from an already-derived
// id can only work with what that derivation kept, and for Telegram
// that is a handle the user can change.
//
// Fails OPEN to fallbackID, deliberately. An unbound address becoming
// its own principal is today's behaviour and lets a new person talk to
// the bot without an operator editing a file first; a lookup error is
// logged by the caller and must not lock somebody out of their own
// history.
func (r *Resolver) ResolveChannel(ctx context.Context, channel, address, fallbackID string) (Principal, error) {
	if r != nil && r.bindings != nil && channel != "" && address != "" {
		userID, err := r.bindings.PrincipalFor(ctx, channel, address)
		if err != nil {
			return r.Resolve(fallbackID), err
		}
		if userID != "" {
			return User(userID), nil
		}
	}
	return r.Resolve(fallbackID), nil
}

// Aliases returns the configured mappings, sorted, for logging at boot.
// Operators need to see what the node believes about identity: a typo
// in an alias key fails silently and open — the id simply resolves to
// itself and that person quietly stops seeing their own history.
func (r *Resolver) Aliases() []string {
	if r == nil || len(r.aliases) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.aliases))
	for from, to := range r.aliases {
		out = append(out, from+" -> "+to.String())
	}
	sort.Strings(out)
	return out
}
