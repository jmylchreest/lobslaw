package memory

import (
	"fmt"
	"io"
	stdlog "log"
	"log/slog"

	"github.com/hashicorp/go-hclog"
)

// hclogToSlog adapts an *slog.Logger to hashicorp/go-hclog's Logger
// interface so hashicorp/raft's structured logs flow through our
// existing slog pipeline (level filtering, log filters, JSON
// formatting). Trace maps to slog Debug — slog has no trace level
// and raft's trace stream is genuinely fine-grained.
type hclogToSlog struct {
	base    *slog.Logger
	name    string
	implied []any
}

func newHCLogAdapter(base *slog.Logger, name string) hclog.Logger {
	if base == nil {
		base = slog.Default()
	}
	return &hclogToSlog{base: base.With(slog.String("subsystem", name)), name: name}
}

func (h *hclogToSlog) emit(level slog.Level, msg string, args []any) {
	if !h.base.Enabled(nil, level) {
		return
	}
	merged := args
	if len(h.implied) > 0 {
		merged = append(append([]any{}, h.implied...), args...)
	}
	h.base.Log(nil, level, msg, resolveHCLogArgs(merged)...)
}

// resolveHCLogArgs walks a hclog-style key/value slice and unwraps
// hclog.Format deferred-format values into rendered strings. Without
// this, `hclog.Fmt("%+v", x)` reaches slog as a raw `hclog.Format`
// slice and is printed as `[%+v {...}]` (the format-string and args
// concatenated), which is what hashicorp/raft's "initial
// configuration" line was leaking before this fix.
func resolveHCLogArgs(args []any) []any {
	if len(args) == 0 {
		return args
	}
	out := make([]any, len(args))
	for i, a := range args {
		if f, ok := a.(hclog.Format); ok && len(f) > 0 {
			format, _ := f[0].(string)
			out[i] = fmt.Sprintf(format, f[1:]...)
			continue
		}
		out[i] = a
	}
	return out
}

func (h *hclogToSlog) Log(level hclog.Level, msg string, args ...any) {
	h.emit(toSlogLevel(level), msg, args)
}
func (h *hclogToSlog) Trace(msg string, args ...any) { h.emit(slog.LevelDebug, msg, args) }
func (h *hclogToSlog) Debug(msg string, args ...any) { h.emit(slog.LevelDebug, msg, args) }
func (h *hclogToSlog) Info(msg string, args ...any)  { h.emit(slog.LevelInfo, msg, args) }
func (h *hclogToSlog) Warn(msg string, args ...any)  { h.emit(slog.LevelWarn, msg, args) }
func (h *hclogToSlog) Error(msg string, args ...any) { h.emit(slog.LevelError, msg, args) }

func (h *hclogToSlog) IsTrace() bool { return h.base.Enabled(nil, slog.LevelDebug) }
func (h *hclogToSlog) IsDebug() bool { return h.base.Enabled(nil, slog.LevelDebug) }
func (h *hclogToSlog) IsInfo() bool  { return h.base.Enabled(nil, slog.LevelInfo) }
func (h *hclogToSlog) IsWarn() bool  { return h.base.Enabled(nil, slog.LevelWarn) }
func (h *hclogToSlog) IsError() bool { return h.base.Enabled(nil, slog.LevelError) }

func (h *hclogToSlog) ImpliedArgs() []any { return h.implied }

func (h *hclogToSlog) With(args ...any) hclog.Logger {
	return &hclogToSlog{
		base:    h.base.With(args...),
		name:    h.name,
		implied: append(append([]any{}, h.implied...), args...),
	}
}

func (h *hclogToSlog) Name() string { return h.name }

func (h *hclogToSlog) Named(name string) hclog.Logger {
	full := name
	if h.name != "" {
		full = h.name + "." + name
	}
	return &hclogToSlog{base: h.base.With(slog.String("subsystem", full)), name: full, implied: h.implied}
}

func (h *hclogToSlog) ResetNamed(name string) hclog.Logger {
	return &hclogToSlog{base: h.base.With(slog.String("subsystem", name)), name: name, implied: h.implied}
}

func (h *hclogToSlog) SetLevel(hclog.Level) {}
func (h *hclogToSlog) GetLevel() hclog.Level {
	switch {
	case h.base.Enabled(nil, slog.LevelDebug):
		return hclog.Debug
	case h.base.Enabled(nil, slog.LevelInfo):
		return hclog.Info
	case h.base.Enabled(nil, slog.LevelWarn):
		return hclog.Warn
	default:
		return hclog.Error
	}
}

func (h *hclogToSlog) StandardLogger(*hclog.StandardLoggerOptions) *stdlog.Logger {
	return stdlog.New(h.StandardWriter(nil), "", 0)
}
func (h *hclogToSlog) StandardWriter(*hclog.StandardLoggerOptions) io.Writer {
	return io.Discard
}

func toSlogLevel(l hclog.Level) slog.Level {
	switch l {
	case hclog.Trace, hclog.Debug:
		return slog.LevelDebug
	case hclog.Info:
		return slog.LevelInfo
	case hclog.Warn:
		return slog.LevelWarn
	case hclog.Error:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
