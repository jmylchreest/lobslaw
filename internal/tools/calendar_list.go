package tools

import (
	"context"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func calendarListDef() *types.ToolDef {
	def := calendarDef("calendar_list", "List selected readable calendar IDs for a connection.", false, calendarProps("connection"), []string{"connection"})
	def.Effects.Network = false
	def.RiskTier = types.RiskReversible
	return def
}
func calendarListHandler(s CalendarOperations) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		var q struct {
			Connection string `json:"connection"`
		}
		if err := calendarArguments(args, &q); err != nil {
			return nil, 1, err
		}
		v, err := s.Calendars(ctx, q.Connection)
		return calendarResult(v, err)
	}
}
