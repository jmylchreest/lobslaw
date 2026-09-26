package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
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
	return flagSetWithHelpOutput(name, handling, os.Stderr)
}

func flagSetWithHelpOutput(name string, handling flag.ErrorHandling, help io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, handling)
	fs.SetOutput(logging.Writer(slog.Default(), slog.LevelError))
	setFlagUsage(fs, help, fmt.Sprintf("Usage of %s:\n", name))
	return fs
}

// setFlagUsage keeps custom help on the same plain-text, sanitized output path.
func setFlagUsage(fs *flag.FlagSet, help io.Writer, introduction string) {
	fs.Usage = func() {
		// Usage is command output, not an error. Render it as one unit so any
		// credential-shaped defaults can be sanitized before writing plain text.
		var buf bytes.Buffer
		previous := fs.Output()
		fs.SetOutput(&buf)
		defer fs.SetOutput(previous)
		_, _ = io.WriteString(&buf, introduction)
		fs.PrintDefaults()
		_, _ = io.WriteString(help, logging.SanitizeText(buf.String()))
	}
}
