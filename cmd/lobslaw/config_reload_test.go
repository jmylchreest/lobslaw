package main

import (
	"log/slog"
	"testing"

	logfilter "github.com/jmylchreest/slog-logfilter"

	"github.com/jmylchreest/lobslaw/internal/logging"
	"github.com/jmylchreest/lobslaw/pkg/config"
)

// Built through internal/logging rather than slog directly: the
// package-level filter API is a no-op until logfilter.New has
// installed a global handler, which is what logging.New does.
func discardLogger() *slog.Logger {
	return logging.New(nopWriter{}, slog.LevelInfo, logging.FormatJSON)
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestRestartRequiredSectionsExcludesLogging(t *testing.T) {
	t.Parallel()
	a, b := &config.Config{}, &config.Config{}
	b.Logging.Level = "debug"
	b.Gateway.Enabled = !a.Gateway.Enabled
	got := restartRequiredSections(a, b)
	for _, s := range got {
		if s == "logging" {
			t.Fatalf("logging is applied live and must not be reported: %v", got)
		}
	}
	if len(got) != 1 || got[0] != "gateway" {
		t.Errorf("got %v, want [gateway]", got)
	}
}

func TestLoggingReloadAppliesLevel(t *testing.T) {
	// NOT parallel: logfilter's level is process-global.
	t.Cleanup(func() { logfilter.SetLevel(slog.LevelInfo) })
	old := config.LoggingConfig{Level: "info"}
	next := config.LoggingConfig{Level: "debug"}
	applyLoggingReload(old, next, flags{logSetOnWire: map[string]bool{}}, discardLogger())
	if got := logfilter.GetLevel(); got != slog.LevelDebug {
		t.Errorf("level = %v, want debug from the file", got)
	}
}

func TestLoggingReloadLeavesTheFlagAlone(t *testing.T) {
	// NOT parallel: logfilter's level is process-global.
	// Logger first: logging.New installs a fresh global handler at its
	// own level, so building one after SetLevel would undo it.
	logger := discardLogger()
	logfilter.SetLevel(slog.LevelError)
	t.Cleanup(func() { logfilter.SetLevel(slog.LevelInfo) })
	old := config.LoggingConfig{Level: "info"}
	next := config.LoggingConfig{Level: "debug"}
	// Somebody booted with --log-level to debug THIS process; the file
	// changing underneath them must not take their level away.
	applyLoggingReload(old, next, flags{
		logLevel:     "error",
		logSetOnWire: map[string]bool{"log-level": true},
	}, logger)
	if got := logfilter.GetLevel(); got != slog.LevelError {
		t.Errorf("level = %v, want the flag's error level to survive", got)
	}
}

func TestLoggingReloadClearsRemovedFilters(t *testing.T) {
	// NOT parallel: logfilter's filter set is process-global.
	t.Cleanup(logfilter.ClearFilters)
	logger := discardLogger()
	old := config.LoggingConfig{Filters: []config.LogFilterConfig{
		{Type: "component", Pattern: "raft", Level: "debug", Enabled: true},
	}}
	applyLogFilters(old.Filters, logger)
	if len(logfilter.GetFilters()) == 0 {
		t.Fatal("precondition: filter was not installed")
	}
	// Deleting every filter from the file must remove them. applyLogFilters
	// no-ops on empty, which is right at boot and wrong on reload.
	applyLoggingReload(old, config.LoggingConfig{}, flags{logSetOnWire: map[string]bool{}}, discardLogger())
	if got := logfilter.GetFilters(); len(got) != 0 {
		t.Errorf("filters survived their removal from the file: %v", got)
	}
}
