package mcp

import (
	"time"
)

const (
	sseFrameQueueCapacity    int           = 16
	sseErrorPreviewBytes     int64         = 512
	sseInitialBufferBytes    int           = 64 * 1024
	sseMaxFrameBytes         int           = 4 * 1024 * 1024
	sseResponseDrainBytes    int64         = 1 << 20
	dependencyInstallTimeout time.Duration = 5 * time.Minute
)
