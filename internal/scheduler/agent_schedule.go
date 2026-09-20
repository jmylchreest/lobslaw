package scheduler

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// AgentTurnHandlerRef identifies scheduled agent turns, which incur a fresh
// turn budget on each firing. Internal maintenance handlers are not subject
// to the agent recurrence floor.
const AgentTurnHandlerRef = "agent:turn"

// ParseAgentSchedule accepts standard minute-resolution cron, including
// descriptors, but rejects sub-minute recurrence. Check the interval itself,
// not the time until the first firing: an hourly job can be due in a second.
func ParseAgentSchedule(expr string) (cron.Schedule, error) {
	schedule, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, fmt.Errorf("parse schedule %q: %w", expr, err)
	}
	// Five-field cron and calendar descriptors have minute resolution. Only
	// @every can express a shorter recurrence (including with a TZ prefix).
	if interval, ok := schedule.(cron.ConstantDelaySchedule); ok && interval.Delay < time.Minute {
		return nil, fmt.Errorf("agent schedules must recur at least one minute apart (got %q)", expr)
	}
	return schedule, nil
}
