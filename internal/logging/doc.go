// Package logging wraps log/slog with lobslaw conventions: JSON handler
// by default, text handler when stderr is a TTY, and a WithComponent
// helper that annotates log records with the subsystem name.
//
// Named "logging" rather than "log" to avoid shadowing the stdlib.
package logging
