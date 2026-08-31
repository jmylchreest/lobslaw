package memory

import (
	"time"

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

// DreamChallenge is one unresolved contradiction, phrased for a person.
type DreamChallenge struct {
	// ID is the consolidation the question came from, so an answer
	// can be traced to the verdict that prompted it.
	ID string
	// Question is the adjudicator's own words. Written as a question
	// a person can answer, which is what the prompt asks it for.
	Question string
	// Sides are the memories in disagreement, newest first.
	Sides []*lobslawv1.EpisodicRecord
}

// UnresolvedChallenges returns this principal's live contradictions.
//
// "Live" is doing real work: a conflict whose sides no longer both
// exist has been resolved, whether by the user correcting one, by
// forgetting one, or by a later merge. There is no resolved flag to
// keep in step — the records themselves are the state, so a question
// stops being asked the moment it stops being a question.
func UnresolvedChallenges(store *Store, owner string, limit int) ([]DreamChallenge, error) {
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

	out := make([]DreamChallenge, 0, len(verdicts))
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
		out = append(out, DreamChallenge{
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

// LastDreamRun is when a dream pass last completed.
//
// Read from the session records the pass writes for itself, so there
// is no second place for "when did this last run" to be wrong. Zero
// time means it has never run — a fresh node, or one where dream is
// switched off.
func LastDreamRun(store *Store) (time.Time, error) {
	if store == nil {
		return time.Time{}, nil
	}
	var latest time.Time
	err := store.ForEachPrefix(BucketEpisodicRecords, dreamSessionPrefix,
		func(_ string, raw []byte) error {
			var rec lobslawv1.EpisodicRecord
			if err := proto.Unmarshal(raw, &rec); err != nil {
				return nil //nolint:nilerr // one unreadable session must not hide the rest
			}
			if rec.Timestamp != nil && rec.Timestamp.AsTime().After(latest) {
				latest = rec.Timestamp.AsTime()
			}
			return nil
		})
	if err != nil {
		return time.Time{}, err
	}
	return latest, nil
}

// ChallengeOwners is every principal with a live contradiction.
//
// The list, not the questions: the caller deciding whether somebody
// needs to be spoken to does not need to read their memories to
// decide it.
func ChallengeOwners(store *Store) ([]string, error) {
	if store == nil {
		return nil, nil
	}
	verdicts, err := ListConsolidations(store, ConsolidationQuery{
		Verdict: string(VerdictConflict),
	})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, v := range verdicts {
		owner := v.GetOwner()
		if owner == "" || seen[owner] {
			continue
		}
		// Liveness re-checked per owner rather than trusted from the
		// verdict: a conflict both of whose sides are gone has been
		// settled, and scheduling a message about it would ask a
		// question that no longer exists.
		live, err := UnresolvedChallenges(store, owner, 1)
		if err != nil || len(live) == 0 {
			continue
		}
		seen[owner] = true
		out = append(out, owner)
	}
	return out, nil
}

// HasPendingCommitment reports whether this principal is already due
// to hear from the agent before the given time.
//
// Used to decide whether a challenge needs a message of its own. If
// something is already scheduled to reach them, the question can ride
// on the conversation that starts — one more thing the agent is
// already going to say beats a second notification.
func HasPendingCommitment(store *Store, owner string, before time.Time) (bool, error) {
	if store == nil || owner == "" {
		return false, nil
	}
	found := false
	err := store.ForEach(BucketCommitments, func(_ string, raw []byte) error {
		if found {
			return nil
		}
		var c lobslawv1.AgentCommitment
		if err := proto.Unmarshal(raw, &c); err != nil {
			return nil //nolint:nilerr // one unreadable commitment must not hide the rest
		}
		if c.GetStatus() != "pending" || c.GetOwner() != owner || c.GetDueAt() == nil {
			return nil
		}
		if c.GetDueAt().AsTime().Before(before) {
			found = true
		}
		return nil
	})
	return found, err
}
