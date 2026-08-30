package compute

import (
	"context"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// CrossOwnerAuthorizer decides whether a turn may read records owned
// by someone other than the caller.
//
// It exists so that "operator" is not a magic string in a reader. The
// obvious implementation of an operator role is a HasRole check at the
// point of the read, and that version is wrong in a way that is hard
// to see: it makes the role itself the grant, so the only two states a
// deployment can express are "this person reads nothing of anyone
// else's" and "this person reads everything, always, silently". An
// operator who wants their own access narrowed to one owner, or gated
// behind a confirmation, or revoked for a week, has nowhere to say so.
//
// Routing the same question through the policy engine puts it where
// every other authorisation question already lives: the widening is a
// rule with a subject, an effect, and a priority, so it can be denied,
// scoped by [scope], conditioned, or removed without a code change.
//
// The interface is here rather than in internal/policy so the reading
// paths depend on the question, not on the machinery that answers it —
// which is also what lets a test hand them a two-line fake instead of
// standing up a rule store.
type CrossOwnerAuthorizer interface {
	// AllowsAny reports whether this caller may read across ownership
	// boundaries. Implementations are expected to fail closed: an
	// error, an unreachable rule store, or anything short of an
	// explicit allow must come back false.
	AllowsAny(ctx context.Context, claims *types.Claims) bool
}

// readAudience is the single place a read decides who it is for.
//
// A nil authorizer never widens. This is the load-bearing default: a
// deployment that has not wired one is not making a statement about
// operators, and reading silence as universal read would turn an
// incomplete wiring into a data breach that nothing in the logs
// distinguishes from normal traffic.
func readAudience(ctx context.Context, turn turn.Identity, authz CrossOwnerAuthorizer) memory.Audience {
	if authz != nil && authz.AllowsAny(ctx, turn.Claims()) {
		return memory.Everyone()
	}
	// A conversation several people can read gets the records that
	// conversation produced, on top of the speaker's own. Not the other
	// way round: this WIDENS a shared channel to its own history, it
	// never narrows a DM, and it never crosses between two channels.
	if turn.Shared {
		if key := turn.SessionKey(); key.Channel != "" && key.ChannelID != "" {
			return memory.ForConversation(turn.Principal, key.Channel+":"+key.ChannelID)
		}
	}
	return memory.For(turn.Principal)
}

// forgetRequester renders the principal a Forget runs on behalf of.
//
// A widened caller resolves to the empty requester, which is the
// unrestricted path memory.Service already reserves for callers that
// hold the whole store. The RPC has one field for "whose read is
// this", so a caller policy has excused from ownership filtering has
// nothing to put in it. The principal is not lost by that: the
// authorizer names it in the audit event before this returns, which
// is where a destructive cross-owner operation needed to be recorded
// anyway.
func forgetRequester(ctx context.Context, turn turn.Identity, authz CrossOwnerAuthorizer) string {
	if authz != nil && authz.AllowsAny(ctx, turn.Claims()) {
		return ""
	}
	return turn.Principal.String()
}
