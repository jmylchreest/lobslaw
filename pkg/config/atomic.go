package config

import "sync/atomic"

// AtomicConfig is a typed wrapper around sync/atomic.Pointer for
// subsystems that want lock-free read access to a config value that
// the watcher can replace at runtime. Zero value is a valid empty
// holder; Load returns nil until something is Stored.
type AtomicConfig[T any] struct {
	ptr atomic.Pointer[T]
}

func (a *AtomicConfig[T]) Load() *T     { return a.ptr.Load() }
func (a *AtomicConfig[T]) Store(v *T)   { a.ptr.Store(v) }
