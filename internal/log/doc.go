// Package log wraps log/slog with lobslaw conventions: JSON handler
// by default, text handler when stderr is a TTY, and a WithComponent
// helper that annotates log records with the subsystem name.
package log
