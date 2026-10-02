package discovery

import (
	"time"
)

const (
	// DefaultDialTimeout bounds each peer attempt during discovery and joining.
	DefaultDialTimeout        time.Duration = 5 * time.Second
	defaultBroadcastInterval  time.Duration = 30 * time.Second
	broadcastMaxDatagramBytes int           = 4096
	broadcastReadPollInterval time.Duration = time.Second
)
