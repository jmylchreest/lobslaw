package tools

import (
	"context"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func calendarAgendaDef() *types.ToolDef {
	return calendarDef("calendar_agenda", "Read across all selected agenda or availability calendars. Each result retains calendar nickname/account and reports failures or truncation. Never claim a slot is free from incomplete results. start/end require RFC3339 offsets; mode is agenda (default) or availability.", false, calendarProps("start", "end", "mode"), []string{"start", "end"})
}
func calendarAgendaHandler(s CalendarOperations) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		var q struct {
			Start string `json:"start"`
			End   string `json:"end"`
			Mode  string `json:"mode"`
		}
		if err := calendarArguments(args, &q); err != nil {
			return nil, 1, err
		}
		v, err := s.Agenda(ctx, q.Start, q.End, q.Mode)
		return calendarResult(v, err)
	}
}
