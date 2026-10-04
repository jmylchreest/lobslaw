package oauth

import (
	"time"
)

const (
	defaultDevicePollInterval time.Duration = 5 * time.Second
	defaultDeviceCodeLifetime time.Duration = 30 * time.Minute
	deviceSlowdownFactor      time.Duration = 2
	credentialPersistTimeout  time.Duration = 10 * time.Second
	maxErrorPreviewRunes      int           = 256
)
