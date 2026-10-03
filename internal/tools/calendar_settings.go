package tools

import (
	"context"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func calendarSettingsDef() *types.ToolDef {
	d := calendarDef("calendar_settings", "List your selected calendars, account identities, nicknames, read/write grants, agenda/availability inclusion, purpose defaults and time zone.", false, calendarProps(), []string{})
	d.Effects.Network = false
	return d
}
func calendarSettingsHandler(s CalendarOperations) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		var q struct{}
		if err := calendarArguments(args, &q); err != nil {
			return nil, 1, err
		}
		v, err := s.Inventory(ctx)
		return calendarResult(v, err)
	}
}
