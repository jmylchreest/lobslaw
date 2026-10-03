package tools

import (
	"context"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func calendarEventCreateDef() *types.ToolDef {
	p, r := calendarMutationProps(false)
	return calendarDef("calendar_event_create", "Create a guest-free event after exact human confirmation. Supply title and start/end; date means all-day (exclusive end), dateTime requires an RFC3339 offset. No invitations, deletion or series edits.", true, p, r)
}
func calendarEventCreateHandler(s CalendarOperations) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		m, err := CalendarMutation(args)
		if err != nil {
			return nil, 1, err
		}
		preview, err := s.Prepare(ctx, "create", m)
		if err != nil {
			return nil, 1, err
		}
		v, err := s.Apply(ctx, preview.ID)
		return calendarResult(v, err)
	}
}
