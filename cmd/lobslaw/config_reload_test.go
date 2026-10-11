package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	logfilter "github.com/jmylchreest/slog-logfilter"

	"github.com/jmylchreest/lobslaw/internal/logging"
	"github.com/jmylchreest/lobslaw/pkg/config"
)

// Use the same filtering handler as the application.
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
	t.Parallel()
	old := config.LoggingConfig{Level: "info"}
	next := config.LoggingConfig{Level: "debug"}
	logger := discardLogger()
	applyLoggingReload(old, next, flags{logSetOnWire: map[string]bool{}}, logger)
	if got := logger.Handler().(*logfilter.Handler).GetLevel(); got != slog.LevelDebug {
		t.Errorf("level = %v, want debug from the file", got)
	}
}

func TestLoggingReloadLeavesTheFlagAlone(t *testing.T) {
	t.Parallel()
	logger := discardLogger()
	logger.Handler().(*logfilter.Handler).SetLevel(slog.LevelError)
	old := config.LoggingConfig{Level: "info"}
	next := config.LoggingConfig{Level: "debug"}
	// Somebody booted with --log-level to debug THIS process; the file
	// changing underneath them must not take their level away.
	applyLoggingReload(old, next, flags{
		logLevel:     "error",
		logSetOnWire: map[string]bool{"log-level": true},
	}, logger)
	if got := logger.Handler().(*logfilter.Handler).GetLevel(); got != slog.LevelError {
		t.Errorf("level = %v, want the flag's error level to survive", got)
	}
}

func TestLoggingReloadClearsRemovedFilters(t *testing.T) {
	t.Parallel()
	logger := discardLogger()
	old := config.LoggingConfig{Filters: []config.LogFilterConfig{
		{Type: "component", Pattern: "raft", Level: "debug", Enabled: true},
	}}
	applyLogFilters(old.Filters, logger)
	if len(logger.Handler().(*logfilter.Handler).GetFilters()) == 0 {
		t.Fatal("precondition: filter was not installed")
	}
	// Deleting every filter from the file must remove them. applyLogFilters
	// no-ops on empty, which is right at boot and wrong on reload.
	applyLoggingReload(old, config.LoggingConfig{}, flags{logSetOnWire: map[string]bool{}}, logger)
	if got := logger.Handler().(*logfilter.Handler).GetFilters(); len(got) != 0 {
		t.Errorf("filters survived their removal from the file: %v", got)
	}
}

func TestLoggingReloadUpdatesChildrenAndIsolatesOtherLoggers(t *testing.T) {
	t.Parallel()
	var output, otherOutput bytes.Buffer
	logger := logging.New(&output, slog.LevelInfo, logging.FormatJSON)
	child := logger.With("component", "worker")
	other := logging.New(&otherOutput, slog.LevelInfo, logging.FormatJSON)
	flags := flags{logSetOnWire: map[string]bool{}}
	old := config.LoggingConfig{Level: "info"}
	next := config.LoggingConfig{Level: "debug"}
	applyLoggingReload(old, next, flags, logger)
	output.Reset()
	child.Debug("child enabled")
	other.Debug("other suppressed")
	if !strings.Contains(output.String(), "child enabled") || otherOutput.Len() != 0 {
		t.Fatalf("level reload: child=%s other=%s", output.String(), otherOutput.String())
	}
	filtered := config.LoggingConfig{Level: "info", Filters: []config.LogFilterConfig{
		{Type: "component", Pattern: "worker", Level: "debug", Enabled: true},
	}}
	applyLoggingReload(next, filtered, flags, logger)
	output.Reset()
	child.Debug("filter enabled")
	other.With("component", "worker").Debug("other suppressed")
	if !strings.Contains(output.String(), "filter enabled") || otherOutput.Len() != 0 {
		t.Fatalf("filter reload: child=%s other=%s", output.String(), otherOutput.String())
	}
	applyLoggingReload(filtered, old, flags, logger)
	output.Reset()
	child.Debug("filter removed")
	if output.Len() != 0 {
		t.Fatalf("child retained removed filter: %s", output.String())
	}
}
