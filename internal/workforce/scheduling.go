package workforce

import (
	"context"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// ScheduledRun uses the scheduler's persisted occurrence time, not wall clock,
// so takeover of a scheduler claim cannot instantiate a second task.
func (s *Service) ScheduledRun(ctx context.Context, owner, id, event, approvedDigest string, claims *types.Claims) error {
	st, e := s.find(ctx, owner, id, "routine")
	if e != nil {
		return e
	}
	return s.mutate(ctx, owner, st.Project.ID, func(st *State) error {
		key := "schedule:" + id + ":" + event
		if st.Events[key] != "" {
			return nil
		}
		if len(st.Events) >= MaxRecords {
			return ErrInvalid
		}
		r := st.Routines[id]
		if r == nil || r.ApprovedDigest != approvedDigest {
			return ErrForbidden
		}
		task, e := s.routineTask(ctx, st, r, claims)
		if e != nil {
			return e
		}
		st.Events[key] = task.ID
		return nil
	})
}
