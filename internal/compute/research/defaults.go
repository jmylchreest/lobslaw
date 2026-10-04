package research

import "time"

const (
	// DefaultDepth bounds work when a caller omits the research depth.
	DefaultDepth int = 3
	// MaxDepth caps planner fan-out and the resulting provider spend.
	MaxDepth                   int           = 10
	workerTimeout              time.Duration = 90 * time.Second
	workerMaxToolCalls         int           = 8
	notificationReportMaxBytes int           = 3500
	synthesisTemperature       float32       = 0.2
)
