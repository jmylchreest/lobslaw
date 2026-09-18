package egress

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	logfilter "github.com/jmylchreest/slog-logfilter"

	"github.com/jmylchreest/lobslaw/internal/logging"
)

func TestDependencyLoggerUsesSharedPipeline(t *testing.T) {
	var out bytes.Buffer
	l := logging.New(&out, slog.LevelInfo, logging.FormatJSON)
	bridge := logrusFromSlog(l.With("component", "egress"))
	bridge.WithField("password", "hidden-value").Warn("failed Bearer secret-value")
	if strings.Contains(out.String(), "hidden-value") || strings.Contains(out.String(), "secret-value") || !strings.Contains(out.String(), "egress") {
		t.Fatal(out.String())
	}
	out.Reset()
	logfilter.SetLevel(slog.LevelError)
	bridge.Warn("suppressed")
	if out.Len() != 0 {
		t.Fatal(out.String())
	}
	logfilter.SetLevel(slog.LevelDebug)
	bridge.Debug("visible debug")
	if !strings.Contains(out.String(), "visible debug") {
		t.Fatal(out.String())
	}
}
