package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// RaftPrompts is the durable Prompts implementation. A confirmation
// issued by one node can be answered on another, and survives the
// asking process restarting — neither of which the in-memory registry
// could do.
type RaftPrompts struct {
	store *memory.PromptStore
	// nodeID is recorded as the resolver. The channel handlers do not
	// carry the answering user's identity into Resolve, so this is the
	// coarsest true answer to "who closed this" — better in the audit
	// trail than an empty string that reads like nobody did.
	nodeID string
	// caps are this node's current budget limits, applied when a
	// paused turn is rebuilt. Read from config rather than from the
	// record, so an operator lowering a limit is not overridden by a
	// turn that started before the change.
	caps compute.BudgetCaps
}

// NewRaftPrompts wraps a raft-backed store as the gateway registry.
func NewRaftPrompts(store *memory.PromptStore, nodeID string, caps compute.BudgetCaps) *RaftPrompts {
	return &RaftPrompts{store: store, nodeID: nodeID, caps: caps}
}

func (r *RaftPrompts) Create(np NewPrompt) (*Prompt, error) {
	rec := &lobslawv1.PromptRecord{
		TurnId:       np.TurnID,
		SessionId:    np.SessionID,
		Reason:       np.Reason,
		Channel:      np.Channel,
		ChannelId:    np.ChannelID,
		Action:       np.Action,
		Resource:     np.Resource,
		Continuation: continuationToProto(np.Continuation),
		// Both carried through raft. Dropping them here is what made a
		// prompt come back unattributable on a real node while every
		// in-memory test passed.
		RaisedFor: np.RaisedFor,
		Enrolment: np.Enrolment,
	}
	if np.TTL > 0 {
		rec.ExpiresAt = timestamppb.New(time.Now().Add(np.TTL))
	}
	out, err := r.store.Create(rec)
	if err != nil {
		return nil, err
	}
	return r.fromRecord(out)
}

func (r *RaftPrompts) Get(id string) (*Prompt, error) {
	rec, err := r.store.Get(id)
	if err != nil {
		return nil, translatePromptErr(err)
	}
	return r.fromRecord(rec)
}

func (r *RaftPrompts) Resolve(id string, decision PromptDecision, scope PromptScope) error {
	if decision != PromptApproved && decision != PromptDenied {
		return errors.New("prompt: Resolve accepts only Approved or Denied")
	}
	// A denial has no scope worth recording: "no, and never again" is
	// not something any button offers, and storing one would read as
	// a standing refusal nobody asked for.
	if decision == PromptDenied {
		scope = PromptScopeOnce
	}
	_, err := r.store.Resolve(id, toDecision(decision), toScope(scope), r.nodeID)
	return translatePromptErr(err)
}

func (r *RaftPrompts) Wait(ctx context.Context, id string) (PromptDecision, error) {
	rec, err := r.store.Wait(ctx, id, 0)
	if err != nil {
		return PromptPending, translatePromptErr(err)
	}
	return fromDecision(rec.Decision), nil
}

func (r *RaftPrompts) fromRecord(rec *lobslawv1.PromptRecord) (*Prompt, error) {
	p := &Prompt{
		ID:        rec.Id,
		TurnID:    rec.TurnId,
		SessionID: rec.SessionId,
		Reason:    rec.Reason,
		Channel:   rec.Channel,
		ChannelID: rec.ChannelId,
		Action:    rec.Action,
		Resource:  rec.Resource,
		Decision:  fromDecision(rec.Decision),
		Scope:     fromScope(rec.Scope),
		RaisedFor: rec.RaisedFor,
		Enrolment: rec.Enrolment,
	}
	if rec.CreatedAt != nil {
		p.CreatedAt = rec.CreatedAt.AsTime()
	}
	if rec.ExpiresAt != nil {
		p.ExpiresAt = rec.ExpiresAt.AsTime()
	}
	cont, err := continuationFromProto(rec.Continuation, r.caps)
	if err != nil {
		return nil, fmt.Errorf("prompt %q: rebuild continuation: %w", rec.Id, err)
	}
	p.Continuation = cont
	return p, nil
}

func toScope(s PromptScope) lobslawv1.PromptScope {
	switch s {
	case PromptScopeSession:
		return lobslawv1.PromptScope_PROMPT_SCOPE_SESSION
	case PromptScopeAlways:
		return lobslawv1.PromptScope_PROMPT_SCOPE_ALWAYS
	default:
		return lobslawv1.PromptScope_PROMPT_SCOPE_ONCE
	}
}

func fromScope(s lobslawv1.PromptScope) PromptScope {
	switch s {
	case lobslawv1.PromptScope_PROMPT_SCOPE_SESSION:
		return PromptScopeSession
	case lobslawv1.PromptScope_PROMPT_SCOPE_ALWAYS:
		return PromptScopeAlways
	default:
		return PromptScopeOnce
	}
}

func toDecision(d PromptDecision) lobslawv1.PromptDecision {
	switch d {
	case PromptApproved:
		return lobslawv1.PromptDecision_PROMPT_DECISION_APPROVED
	case PromptDenied:
		return lobslawv1.PromptDecision_PROMPT_DECISION_DENIED
	case PromptTimedOut:
		return lobslawv1.PromptDecision_PROMPT_DECISION_TIMED_OUT
	default:
		return lobslawv1.PromptDecision_PROMPT_DECISION_PENDING
	}
}

func fromDecision(d lobslawv1.PromptDecision) PromptDecision {
	switch d {
	case lobslawv1.PromptDecision_PROMPT_DECISION_APPROVED:
		return PromptApproved
	case lobslawv1.PromptDecision_PROMPT_DECISION_DENIED:
		return PromptDenied
	case lobslawv1.PromptDecision_PROMPT_DECISION_TIMED_OUT:
		return PromptTimedOut
	default:
		return PromptPending
	}
}

// translatePromptErr maps the store's sentinels onto the gateway's,
// which the channel handlers already switch on to pick user-facing
// wording. Anything else passes through untouched — collapsing an
// unknown failure into a known one is how "couldn't reach the leader"
// becomes "that prompt no longer exists".
func translatePromptErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, memory.ErrPromptNotFound):
		return ErrPromptNotFound
	case errors.Is(err, memory.ErrPromptResolved):
		return ErrPromptResolved
	default:
		return err
	}
}
