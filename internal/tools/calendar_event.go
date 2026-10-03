package tools

import (
	"context"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/google/calendar"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func calendarEventDef() *types.ToolDef {
	return calendarDef("calendar_event", "Read one event by its exact ID.", false, calendarProps("connection", "calendar", "event"), []string{"connection", "calendar", "event"})
}
func calendarEventHandler(s CalendarOperations) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		var q calendar.Query
		if err := calendarArguments(args, &q); err != nil {
			return nil, 1, err
		}
		v, err := s.Event(ctx, q)
		return calendarResult(v, err)
	}
}
