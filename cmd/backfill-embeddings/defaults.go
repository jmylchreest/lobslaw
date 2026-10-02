package main

import "time"

const (
	defaultEmbeddingRPM     int           = 10
	embeddingMaxAttempts    int           = 5
	embeddingRequestTimeout time.Duration = time.Minute
	embeddingInitialBackoff time.Duration = 5 * time.Second
	embeddingMaxBackoff     time.Duration = time.Minute
	modelLoadTimeout        time.Duration = 30 * time.Minute
)
