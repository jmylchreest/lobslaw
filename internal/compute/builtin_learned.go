package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// What the agent taught itself, readable by the agent.
//
// R12 shipped memory transparency so a person can see what the agent
// wrote for itself. It is operator-facing only — CLI over mTLS — and
// the primary interface here is a chat channel. Asked in that channel
// whether it had proposed anything, the assistant reached for the
// nearest plausible tool (memory_recent), queried the episodic store,
// got a truthful empty, and reported it. Two proposals were sitting
// in a bucket it had no way to name.
//
// "The agent cannot see what it proposed" is a defensible design.
// "The agent answers no when the answer is two" is not.
//
// skill_view cannot see them either, and that part IS correct:
// materialisation is for ACTIVE artefacts, and in propose mode they
// land PROPOSED and inert. So they are absent from the skill index by
// design, which is the same reason they need their own reader.

// LearnedLister is the subset of the self-taught store this builtin
// needs. Narrow on purpose: listing is the whole capability, and a
// wider interface here would be an invitation to widen the tool.
type LearnedLister interface {
	// ListForAgent returns the live set — proposed and active — as a
	// summary per artefact. Never the archive: an artefact somebody
	// archived is a decision already taken, and re-surfacing it to the
	// thing that proposed it invites re-proposing.
	ListForAgent(ctx context.Context, owner string) ([]LearnedSummary, error)
}

// LearnedSummary is one artefact, as much as the agent needs to talk
// about it and no more.
//
// No body. The listing answers "what have I written and where does it
// stand"; the body is what a human reads before approving, and
// `lobslaw learned show` is where that happens. Sending it here would
// put the agent's own instructions back into its context every time
// it was asked a question about itself.
type LearnedSummary struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	State       string `json:"state"`
	TurnID      string `json:"turn_id,omitempty"`
	// Pending reports that a refinement is staged against a live
	// artefact — a different thing from the artefact being proposed,
	// and the distinction an operator needs before running accept.
	Pending bool `json:"pending_refinement,omitempty"`
}

// LearnedConfig wires the builtin. A nil Store disables it: absence
// rather than a tool that answers "not configured", which reads to
// the model as a transient failure worth retrying.
type LearnedConfig struct {
	Store LearnedLister
}

// RegisterLearnedBuiltins installs learned_list.
func RegisterLearnedBuiltins(b *Builtins, cfg LearnedConfig) error {
	if cfg.Store == nil {
		return errors.New("learned builtins: Store required")
	}
	return b.Register("learned_list", newLearnedListHandler(cfg.Store))
}

// LearnedToolDefs returns the ToolDef entries.
//
// There is no learned_approve, and there should not be. The store's
// narrowness exists so the review fork cannot reach past its own
// namespace; letting the agent approve its own proposals hands back
// exactly what that design withholds. Approval stays operator-only,
// over mTLS or through a durable confirmation prompt — a decision the
// requester cannot also make.
func LearnedToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "learned_list",
			Path:        BuiltinScheme + "learned_list",
			Description: "List the instructions you have written for yourself — skills and notes proposed by the post-turn review, with their state. Use when asked what you have learned, taught yourself, proposed, or what is waiting for approval; these live in their own store and are NOT visible through memory_search, memory_recent or skill_view. State is 'proposed' (written, inert, waiting for a human) or 'active' (approved and in use). You cannot approve them — say who can: the operator, with `lobslaw learned show <id>` to read one and `lobslaw learned approve <id>` to accept it.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {},
				"additionalProperties": false
			}`),
			// Reversible, for the reason skill_view is: listing runs
			// nothing. Reading your own proposals is strictly less
			// sensitive than reading a skill's full instructions,
			// which is already allowed at this tier.
			RiskTier: types.RiskReversible,
		},
	}
}

func newLearnedListHandler(store LearnedLister) BuiltinFunc {
	return func(ctx context.Context, _ map[string]string) ([]byte, int, error) {
		// Owner comes from the TURN, not the arguments. A parameter
		// naming whose artefacts to list is a parameter the model can
		// be talked into changing, and the tool takes no arguments at
		// all for that reason.
		turn, _ := turn.IdentityFrom(ctx)
		items, err := store.ListForAgent(ctx, learnedOwner(turn))
		if err != nil {
			return nil, 1, fmt.Errorf("learned_list: %w", err)
		}
		payload, err := json.Marshal(map[string]any{
			"artefacts": items,
			"count":     len(items),
		})
		if err != nil {
			return nil, 1, err
		}
		return payload, 0, nil
	}
}

// learnedOwner is the owner string in the form the review fork STAMPS
// artefacts with.
//
// ownerOf writes "user:" + Claims.UserID, so a filter built from the
// bare principal matches nothing and the tool answers "you have
// proposed nothing" while two proposals sit in the store — which is
// the failure this builtin exists to fix, reproduced one layer down.
//
// Empty when the turn has no identity, which the store reads as "do
// not filter" rather than "match the empty owner": an anonymous turn
// asking what the assistant has learned should see the assistant's
// artefacts, not an empty list.
func learnedOwner(turn turn.Identity) string {
	if turn.UserID == "" {
		return ""
	}
	return "user:" + turn.UserID
}
