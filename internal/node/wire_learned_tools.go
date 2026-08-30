package node

import (
	"context"
	"fmt"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/tools"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// learnedLister adapts the self-taught store to what the builtin needs.
//
// An adapter rather than passing the store, so the tool's reach is the
// interface and not whatever the store happens to expose next year.
// The store is narrow on purpose — the review fork must not be able to
// approve its own work — and a builtin holding the whole thing would
// quietly undo that.
type learnedLister struct{ store *memory.SelfTaughtStore }

// ListForAgent returns the live set, newest first.
//
// Never the archive. An archived artefact is a decision already taken,
// and re-surfacing it to the thing that proposed it invites
// re-proposing the same idea.
func (l learnedLister) ListForAgent(_ context.Context, owner string) ([]tools.LearnedSummary, error) {
	recs, err := l.store.List(memory.SelfTaughtQuery{Owner: owner})
	if err != nil {
		return nil, err
	}
	out := make([]tools.LearnedSummary, 0, len(recs))
	for _, r := range recs {
		out = append(out, tools.LearnedSummary{
			ID:          r.GetId(),
			Kind:        kindName(r.GetKind()),
			Name:        r.GetName(),
			Description: r.GetDescription(),
			State:       stateName(r.GetState()),
			TurnID:      r.GetTurnId(),
			Pending:     r.GetPending() != nil,
		})
	}
	return out, nil
}

// kindName and stateName render the enums as the words an operator
// types, so what the agent says matches what `lobslaw learned list`
// prints and what the CLI accepts back.
//
// Derived from the enum name rather than switched on value by value:
// a hand-written switch silently returns "unknown" for a kind added
// later, and the first version of this one did exactly that by
// listing a constant that does not exist.
func kindName(k lobslawv1.SelfTaughtKind) string {
	return strings.ToLower(strings.TrimPrefix(k.String(), "SELF_TAUGHT_KIND_"))
}

func stateName(s lobslawv1.SelfTaughtState) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "SELF_TAUGHT_STATE_"))
}

// wireLearnedTools registers learned_list when there is a store to
// read. Absent otherwise — a tool that answers "not configured" reads
// to the model as a transient failure worth retrying.
func (n *Node) wireLearnedTools(builtins *tools.Builtins) error {
	if n.selfTaught == nil || n.toolRegistry == nil {
		return nil
	}
	if err := tools.RegisterLearnedBuiltins(builtins, tools.LearnedConfig{
		Store: learnedLister{store: n.selfTaught},
	}); err != nil {
		return fmt.Errorf("register learned builtins: %w", err)
	}
	for _, td := range tools.LearnedToolDefs() {
		if err := n.toolRegistry.Register(td); err != nil {
			return fmt.Errorf("register learned tool %q: %w", td.Name, err)
		}
	}
	n.log.Debug("compute: learned_list registered")
	return nil
}

// proposalsProvider counts this owner's artefacts awaiting approval,
// for the Installed Skills section of the system prompt.
//
// Nil when there is no store, and that is the whole reason it is a
// provider rather than a number: with self-learning off the section
// says nothing about proposals, instead of asserting zero. "None
// pending" and "the feature is not running" are different statements,
// and only one of them is true.
//
// Counts PROPOSED only. A refinement pending on an already-active
// artefact is a different question with a different answer, and
// folding the two into one number would make the count wrong in the
// way that is hardest to notice — plausible, and quietly off by the
// number of pending refinements.
//
// Errors count as zero rather than propagating. This runs while
// building a prompt, on every turn; failing the turn because a
// supplementary count could not be read would trade an answer for a
// footnote.
func (n *Node) proposalsProvider() func(string) int {
	if n.selfTaught == nil {
		return nil
	}
	return func(owner string) int {
		recs, err := n.selfTaught.List(memory.SelfTaughtQuery{
			Owner: owner,
			State: lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_PROPOSED,
		})
		if err != nil {
			n.log.Debug("compute: proposal count unavailable", "err", err)
			return 0
		}
		return len(recs)
	}
}
