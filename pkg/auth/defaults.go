package auth

import (
	"crypto/sha256"
	"time"
)

const (
	defaultJWKSFetchTimeout    time.Duration = 10 * time.Second
	defaultJWKSRefreshInterval time.Duration = 10 * time.Minute
	defaultJWKSForceRefreshMin time.Duration = 30 * time.Second
	maxJWKSErrorPreviewBytes   int64         = 512
	maxJWKSResponseBytes       int64         = 1 << 20
	// HMAC keys must contain at least as many bits as the SHA-256 output.
	minHS256SecretBytes int = sha256.Size
)
