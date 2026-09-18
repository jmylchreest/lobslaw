package egress

import (
	"context"
	"io"
	"log/slog"

	"github.com/sirupsen/logrus"
)

// Smokescreen requires logrus. This adapter has no output sink or filtering
// policy of its own: every level is forwarded to the supplied slog pipeline.
func logrusFromSlog(l *slog.Logger) *logrus.Logger {
	if l == nil {
		l = slog.Default()
	}
	bridge := logrus.New()
	bridge.SetOutput(io.Discard)
	bridge.SetLevel(logrus.TraceLevel)
	bridge.AddHook(slogForwardHook{logger: l})
	return bridge
}

type slogForwardHook struct{ logger *slog.Logger }

func (slogForwardHook) Levels() []logrus.Level { return logrus.AllLevels }
func (h slogForwardHook) Fire(e *logrus.Entry) error {
	level := slog.LevelInfo
	switch e.Level {
	case logrus.TraceLevel, logrus.DebugLevel:
		level = slog.LevelDebug
	case logrus.WarnLevel:
		level = slog.LevelWarn
	case logrus.ErrorLevel, logrus.FatalLevel, logrus.PanicLevel:
		level = slog.LevelError
	}
	ctx := e.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if !h.logger.Enabled(ctx, level) {
		return nil
	}
	attrs := make([]slog.Attr, 0, len(e.Data))
	for k, v := range e.Data {
		attrs = append(attrs, slog.Any(k, v))
	}
	h.logger.LogAttrs(ctx, level, e.Message, attrs...)
	return nil
}
