package egress

import (
	"time"
)

const (
	proxyReadHeaderTimeout time.Duration = 10 * time.Second
	proxyMaxIdleConns      int           = 100
	proxyIdleConnTimeout   time.Duration = 90 * time.Second
)
