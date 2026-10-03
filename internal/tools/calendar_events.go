package tools

import (
	"context"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/google/calendar"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func calendarEventsDef() *types.ToolDef {
	return calendarDef("calendar_events", "Read up to 100 events within an RFC3339 start/end interval; truncated=true requires a narrower interval.", false, calendarProps("connection", "calendar", "start", "end"), []string{"connection", "calendar", "start", "end"})
}
func calendarEventsHandler(s CalendarOperations) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		var q calendar.Query
		if err := calendarArguments(args, &q); err != nil {
			return nil, 1, err
		}
		v, err := s.Events(ctx, q)
		return calendarResult(v, err)
	}
}
