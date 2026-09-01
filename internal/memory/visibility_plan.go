package memory

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// One definition of what share and unshare do.
//
// The CLI had the only one, computed against a state.db it opened
// itself — which it cannot do while the node is running. Moving the
// plan here lets both callers ask the same question of the same
// store, and keeps the refusal to touch an unowned record in one
// place rather than in whichever path happened to be taken.

// VisibilityChange is one record's before and after.
type VisibilityChange struct {
	ID    string
	Kind  string // vector | episodic
	Owner string
	From  lobslawv1.Visibility
	To    lobslawv1.Visibility

	vector   *lobslawv1.VectorRecord
	episodic *lobslawv1.EpisodicRecord
}

// entry is the log entry that applies this change.
func (c VisibilityChange) entry() *lobslawv1.LogEntry {
	if c.vector != nil {
		v := proto.Clone(c.vector).(*lobslawv1.VectorRecord)
		v.Visibility = c.To
		return &lobslawv1.LogEntry{
			Op:      lobslawv1.LogOp_LOG_OP_PUT,
			Id:      v.Id,
			Payload: &lobslawv1.LogEntry_VectorRecord{VectorRecord: v},
		}
	}
	e := proto.Clone(c.episodic).(*lobslawv1.EpisodicRecord)
	e.Visibility = c.To
	return &lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      e.Id,
		Payload: &lobslawv1.LogEntry_EpisodicRecord{EpisodicRecord: e},
	}
}

// PlanVisibility works out what share or unshare would do.
//
// Refuses the whole request when any id is unknown or unowned, rather
// than doing the rest: a partial answer to "share these five" is one
// the operator has to reconstruct by reading the output carefully,
// and the failure mode of getting it wrong is a memory shared with
// people it was not meant for.
func PlanVisibility(store *Store, ids []string, to lobslawv1.Visibility) ([]VisibilityChange, error) {
	var (
		changes  []VisibilityChange
		unknown  []string
		orphaned []string
	)
	for _, id := range ids {
		change, found, err := planOne(store, id, to)
		if err != nil {
			return nil, err
		}
		switch {
		case !found:
			unknown = append(unknown, id)
		case change.Owner == "":
			orphaned = append(orphaned, id)
		default:
			changes = append(changes, change)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("no vector or episodic record with id: %s", strings.Join(unknown, ", "))
	}
	if len(orphaned) > 0 {
		return nil, fmt.Errorf("refusing to change visibility of unowned record(s): %s — %s",
			strings.Join(orphaned, ", "), unownedNote)
	}
	return changes, nil
}

// unownedNote is the same explanation the CLI gave, kept with the
// refusal it explains.
const unownedNote = "unowned records belong to no principal — investigate before sharing or forgetting them"

func planOne(store *Store, id string, to lobslawv1.Visibility) (VisibilityChange, bool, error) {
	if raw, err := store.Get(BucketVectorRecords, id); err == nil {
		var rec lobslawv1.VectorRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			return VisibilityChange{}, false, fmt.Errorf("decode vector %s: %w", id, err)
		}
		return VisibilityChange{
			ID: id, Kind: "vector", Owner: rec.GetOwner(),
			From: rec.GetVisibility(), To: to, vector: &rec,
		}, true, nil
	}
	if raw, err := store.Get(BucketEpisodicRecords, id); err == nil {
		var rec lobslawv1.EpisodicRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			return VisibilityChange{}, false, fmt.Errorf("decode episodic %s: %w", id, err)
		}
		return VisibilityChange{
			ID: id, Kind: "episodic", Owner: rec.GetOwner(),
			From: rec.GetVisibility(), To: to, episodic: &rec,
		}, true, nil
	}
	return VisibilityChange{}, false, nil
}
