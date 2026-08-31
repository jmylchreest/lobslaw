package memory

import (
	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// The memories that stop Dream sleeping.
//
// A conflict is the one verdict Dream cannot act on: two memories that
// cannot both be true, with nothing in either saying which. Marking it
// at recall time tells the model the ground is uneven. It does not
// make the ground even — only the person whose memories these are can
// do that, and only if somebody asks them.
//
// So an unresolved conflict becomes a question. Not an alert, not a
// queue to go and drain: a line riding out on a conversation that is
// already happening, in the same channel the memories came from.

// Nightmare is one unresolved contradiction, phrased for a person.
type Nightmare struct {
	// ID is the consolidation the question came from, so an answer
	// can be traced to the verdict that prompted it.
	ID string
	// Question is the adjudicator's own words. Written as a question
	// a person can answer, which is what the prompt asks it for.
	Question string
	// Sides are the memories in disagreement, newest first.
	Sides []*lobslawv1.EpisodicRecord
}

// UnresolvedNightmares returns this principal's live contradictions.
//
// "Live" is doing real work: a conflict whose sides no longer both
// exist has been resolved, whether by the user correcting one, by
// forgetting one, or by a later merge. There is no resolved flag to
// keep in step — the records themselves are the state, so a question
// stops being asked the moment it stops being a question.
func UnresolvedNightmares(store *Store, owner string, limit int) ([]Nightmare, error) {
	if store == nil || owner == "" {
		return nil, nil
	}
	verdicts, err := ListConsolidations(store, ConsolidationQuery{
		Owner:   owner,
		Verdict: string(VerdictConflict),
	})
	if err != nil {
		return nil, err
	}

	out := make([]Nightmare, 0, len(verdicts))
	for _, v := range verdicts {
		sides := make([]*lobslawv1.EpisodicRecord, 0, len(v.GetSourceIds()))
		for _, sid := range v.GetSourceIds() {
			raw, err := store.Get(BucketEpisodicRecords, sid)
			if err != nil {
				continue
			}
			var rec lobslawv1.EpisodicRecord
			if err := proto.Unmarshal(raw, &rec); err != nil {
				continue
			}
			sides = append(sides, &rec)
		}
		// One side left is not a disagreement any more.
		if len(sides) < 2 {
			continue
		}
		out = append(out, Nightmare{
			ID:       v.GetId(),
			Question: v.GetReason(),
			Sides:    sides,
		})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}
