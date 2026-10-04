package node

import (
	"context"
	"errors"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// artefactStoreAdapter is the review fork's entire write surface.
//
// Narrow deliberately. The fork's authority is "write on the
// self-taught namespace and nothing else", and an adapter that can
// only reach that store makes the claim structural — there is no
// broader handle to misuse, so it is not a rule anybody has to keep
// enforcing.
type artefactStoreAdapter struct{ inner *memory.SelfTaughtStore }

func (a artefactStoreAdapter) Existing(kind, owner string) ([]compute.ArtefactSummary, error) {
	records, err := a.inner.List(memory.SelfTaughtQuery{Kind: artefactKind(kind), Owner: owner})
	if err != nil {
		return nil, err
	}
	out := make([]compute.ArtefactSummary, 0, len(records))
	for _, r := range records {
		out = append(out, compute.ArtefactSummary{
			ID:          r.Id,
			Name:        r.Name,
			Description: r.Description,
		})
	}
	return out, nil
}

func (a artefactStoreAdapter) Propose(ctx context.Context, art compute.ProposedArtefact) error {
	if art.Owner == "" {
		return errors.New("review: author principal required")
	}
	id := art.Refines
	if id == "" {
		id = strings.ToLower(art.Kind) + ":" + art.Name
	}
	if existing, err := a.inner.Get(id); err == nil && existing.Owner != art.Owner {
		return errors.New("review: cannot refine another principal's skill")
	}
	_, err := a.inner.Propose(ctx, &lobslawv1.SelfTaughtRecord{
		Kind:        artefactKind(art.Kind),
		Name:        art.Name,
		Description: art.Description,
		Body:        art.Body,
		TurnId:      art.TurnID,
		SessionId:   art.SessionID,
		Owner:       art.Owner,
		// Origin is set here rather than taken from the caller: this
		// adapter is only reachable from the fork, so anything
		// arriving through it was decided by the agent on its own.
		Origin: lobslawv1.SelfTaughtOrigin_SELF_TAUGHT_ORIGIN_REVIEW_FORK,
	}, memory.ProposeIntent{
		Refines:   art.Refines,
		Rationale: art.Rationale,
		Distinct:  art.Distinct,
	})
	return err
}

func artefactKind(s string) lobslawv1.SelfTaughtKind {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case compute.ArtefactProcedure:
		return lobslawv1.SelfTaughtKind_SELF_TAUGHT_KIND_PROCEDURE
	default:
		return lobslawv1.SelfTaughtKind_SELF_TAUGHT_KIND_SKILL
	}
}
