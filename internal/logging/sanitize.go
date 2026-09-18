package logging

import (
	"context"
	"io"
	"log"
	"log/slog"
	"regexp"
	"strings"

	logfilter "github.com/jmylchreest/slog-logfilter"
)

// One immutable policy serves logs and errors returned across API boundaries.
// Secrets are recognized by field/context/shape; no credential cache is kept.
var sanitizer = logfilter.NewRedactor(logfilter.RedactorOptions{
	Patterns: []*regexp.Regexp{
		regexp.MustCompile(`/bot[0-9]+(?::|%3[aA])[A-Za-z0-9_-]+`),
		regexp.MustCompile(`\b[0-9]{5,}:[A-Za-z0-9_-]{20,}\b`),
		regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{16,}|sk-[A-Za-z0-9_-]{16,}|xox[baprs]-[A-Za-z0-9-]{10,})\b`),
	},
})

func SanitizeText(s string) string { return sanitizer.SanitizeText(s) }
func SafeError(err error) error    { return logfilter.SanitizeError(err, sanitizer) }

// StandardLogger forwards standard-library diagnostics through the supplied
// logger's handler, preserving the single filtering/sanitization pipeline.
func StandardLogger(l *slog.Logger, level slog.Level) *log.Logger {
	if l == nil {
		l = slog.Default()
	}
	return slog.NewLogLogger(l.Handler(), level)
}

// Writer adapts complete diagnostic writes to the shared logger. It retains no
// buffered fragments and is intended for line-oriented library/flag diagnostics.
func Writer(l *slog.Logger, level slog.Level) io.Writer {
	if l == nil {
		l = slog.Default()
	}
	return diagnosticWriter{logger: l, level: level}
}

type diagnosticWriter struct {
	logger *slog.Logger
	level  slog.Level
}

func (w diagnosticWriter) Write(p []byte) (int, error) {
	w.logger.Log(context.Background(), w.level, SanitizeText(strings.TrimSpace(string(p))))
	return len(p), nil
}
