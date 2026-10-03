package tools

import (
	"context"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func calendarSettingsUpdateDef() *types.ToolDef {
	p := calendarProps("calendar", "value")
	p["operation"] = map[string]any{"type": "string", "enum": []string{"rename", "agenda", "availability", "default", "timezone", "access", "disconnect", "undo"}}
	p["enabled"] = map[string]any{"type": "boolean"}
	p["revision"] = map[string]any{"type": "integer", "minimum": 0}
	p["permission"] = map[string]any{"type": "object", "properties": map[string]any{"read": map[string]any{"type": "boolean"}, "write": map[string]any{"type": "boolean"}}, "required": []string{"read", "write"}, "additionalProperties": false}
	return calendarDef("calendar_settings_update", "Manage your calendar settings. calendar is a nickname or inventory ID. rename uses value; agenda/availability use enabled; default uses purpose in value (enabled=false clears); timezone uses IANA value; access uses independent read/write permission; disconnect removes ALL calendars for that account connection. undo uses returned undo ID in value. Rename and agenda return Undo. All other changes require exact human confirmation; never claim success before the result. These are explicit user preferences, never inferred from event contents or learned skills.", true, p, []string{"operation"})
}
func calendarSettingsUpdateHandler(s CalendarOperations) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		c, err := CalendarSettingsChange(args)
		if err != nil {
			return nil, 1, err
		}
		p, err := s.PrepareSettings(ctx, c)
		if err != nil {
			return nil, 1, err
		}
		v, err := s.ApplySettings(ctx, p.ID)
		return calendarResult(v, err)
	}
}
