package tools

import (
	"context"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func calendarEventUpdateDef() *types.ToolDef {
	p, r := calendarMutationProps(true)
	def := calendarDef("calendar_event_update", "Update a guest-free event after exact human confirmation. Supply title and start/end; date means all-day (exclusive end), dateTime requires an RFC3339 offset. No invitations, deletion or series edits.", true, p, r)
	def.Effects.Reads = true
	return def
}
func calendarEventUpdateHandler(s CalendarOperations) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		m, err := CalendarMutation(args)
		if err != nil {
			return nil, 1, err
		}
		preview, err := s.Prepare(ctx, "update", m)
		if err != nil {
			return nil, 1, err
		}
		v, err := s.Apply(ctx, preview.ID)
		return calendarResult(v, err)
	}
}
