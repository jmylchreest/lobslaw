package main

import (
	"flag"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/logging"
)

func diagnosticf(format string, args ...any) {
	slog.Error(logging.SanitizeText(strings.TrimSpace(fmt.Sprintf(format, args...))))
}
func diagnosticln(args ...any) {
	slog.Error(logging.SanitizeText(strings.TrimSpace(fmt.Sprintln(args...))))
}
func noticef(format string, args ...any) {
	slog.Info(logging.SanitizeText(strings.TrimSpace(fmt.Sprintf(format, args...))))
}

// Flag parse failures can include user-supplied values. Use the same sanitized
// diagnostics path as command failures, including before node startup.
func newFlagSet(name string, handling flag.ErrorHandling) *flag.FlagSet {
	fs := flag.NewFlagSet(name, handling)
	fs.SetOutput(logging.Writer(slog.Default(), slog.LevelError))
	return fs
}
