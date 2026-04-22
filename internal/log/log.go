package log

import (
	"context"
	"io"
	"log/slog"
	"os"

	"golang.org/x/term"
)

type Format string

const (
	FormatAuto Format = "auto"
	FormatJSON Format = "json"
	FormatText Format = "text"
)

// New returns a slog.Logger writing to w at the given level. Format
// "auto" picks text when w is a TTY file, JSON otherwise.
func New(w io.Writer, level slog.Level, format Format) *slog.Logger {
	return slog.New(pickHandler(w, level, format))
}

func pickHandler(w io.Writer, level slog.Level, format Format) slog.Handler {
	opts := &slog.HandlerOptions{Level: level}
	switch format {
	case FormatText:
		return slog.NewTextHandler(w, opts)
	case FormatJSON:
		return slog.NewJSONHandler(w, opts)
	}
	if f, ok := w.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		return slog.NewTextHandler(w, opts)
	}
	return slog.NewJSONHandler(w, opts)
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
