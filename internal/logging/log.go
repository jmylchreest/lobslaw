package logging

import (
	"context"
	"io"
	"log/slog"
	"os"

	logfilter "github.com/jmylchreest/slog-logfilter"
	"golang.org/x/term"
)

type Format string

const (
	FormatAuto Format = "auto"
	FormatJSON Format = "json"
	FormatText Format = "text"
)

// New returns a filter-aware slog.Logger writing to w at the given
// level. Format "auto" picks text when w is a TTY file, JSON otherwise.
//
// The returned logger uses the global filter handler from
// github.com/jmylchreest/slog-logfilter so callers can mutate filters
// at runtime via the library's package-level API (SetFilters,
// AddFilter, SetLevel, etc.). This supports per-subsystem debug
// enabling without a restart.
func New(w io.Writer, level slog.Level, format Format) *slog.Logger {
	return logfilter.New(
		logfilter.WithOutput(w),
		logfilter.WithLevel(level),
		logfilter.WithFormat(resolveFormat(w, format)),
		logfilter.WithSource(true),
	)
}

func resolveFormat(w io.Writer, format Format) string {
	switch format {
	case FormatJSON:
		return "json"
	case FormatText:
		return "text"
	}
	if f, ok := w.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		return "text"
	}
	return "json"
}

// WithComponent returns a child logger tagged with component=name.
func WithComponent(l *slog.Logger, name string) *slog.Logger {
	return l.With("component", name)
}

type ctxKey struct{}

// Into attaches l to ctx so downstream calls can retrieve it with From.
func Into(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// From extracts the logger from ctx, falling back to slog.Default().
func From(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
