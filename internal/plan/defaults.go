package plan

import "time"

// DefaultApplyTimeout bounds consensus writes unless configured otherwise.
const DefaultApplyTimeout time.Duration = 5 * time.Second
