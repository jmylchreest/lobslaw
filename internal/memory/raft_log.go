package memory

import (
	"context"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"strings"

	"github.com/hashicorp/go-hclog"

	"github.com/jmylchreest/lobslaw/internal/logging"
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
	base = logging.OrDefault(base)
	return &hclogToSlog{base: base.With(slog.String("subsystem", name)), name: name}
}

func (h *hclogToSlog) emit(level slog.Level, msg string, args []any) {
	if !h.base.Enabled(context.Background(), level) {
		return
	}
	merged := args
	if len(h.implied) > 0 {
		merged = append(append([]any{}, h.implied...), args...)
	}
	h.base.Log(context.Background(), level, msg, resolveHCLogArgs(merged)...)
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

func (h *hclogToSlog) IsTrace() bool { return h.base.Enabled(context.Background(), slog.LevelDebug) }
func (h *hclogToSlog) IsDebug() bool { return h.base.Enabled(context.Background(), slog.LevelDebug) }
func (h *hclogToSlog) IsInfo() bool  { return h.base.Enabled(context.Background(), slog.LevelInfo) }
func (h *hclogToSlog) IsWarn() bool  { return h.base.Enabled(context.Background(), slog.LevelWarn) }
func (h *hclogToSlog) IsError() bool { return h.base.Enabled(context.Background(), slog.LevelError) }

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
	case h.base.Enabled(context.Background(), slog.LevelDebug):
		return hclog.Debug
	case h.base.Enabled(context.Background(), slog.LevelInfo):
		return hclog.Info
	case h.base.Enabled(context.Background(), slog.LevelWarn):
		return hclog.Warn
	default:
		return hclog.Error
	}
}

func (h *hclogToSlog) StandardLogger(opts *hclog.StandardLoggerOptions) *stdlog.Logger {
	return stdlog.New(h.StandardWriter(opts), "", 0)
}
func (h *hclogToSlog) StandardWriter(opts *hclog.StandardLoggerOptions) io.Writer {
	var options hclog.StandardLoggerOptions
	if opts != nil {
		options = *opts
	}
	return raftStandardWriter{logger: h, options: options}
}

type raftStandardWriter struct {
	logger  *hclogToSlog
	options hclog.StandardLoggerOptions
}

func (w raftStandardWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), " \t\n")
	level := hclog.Info
	if w.options.InferLevels || w.options.ForceLevel != hclog.NoLevel {
		if w.options.InferLevels && w.options.InferLevelsWithTimestamp && w.options.ForceLevel == hclog.NoLevel {
			msg = strings.TrimLeft(msg, "0123456789 \t\n:/.-+TZ")
		}
		if prefix, rest, ok := strings.Cut(msg, "]"); ok {
			switch prefix {
			case "[TRACE":
				level = hclog.Trace
			case "[DEBUG":
				level = hclog.Debug
			case "[INFO":
				level = hclog.Info
			case "[WARN":
				level = hclog.Warn
			case "[ERR", "[ERROR":
				level = hclog.Error
			default:
				rest = msg
			}
			msg = strings.TrimSpace(rest)
		}
		if w.options.ForceLevel != hclog.NoLevel {
			level = w.options.ForceLevel
		}
	}
	w.logger.Log(level, logging.SanitizeText(msg))
	return len(p), nil
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
