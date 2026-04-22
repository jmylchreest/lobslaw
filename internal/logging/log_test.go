package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestNewFormatJSON(t *testing.T) {
	// No t.Parallel(): logfilter.New uses a process-wide default handler
	// (required for runtime filter mutation via the library's package-
	// level API). Parallel tests would race over that global — we keep
	// these serial instead.
	var buf bytes.Buffer
	logger := New(&buf, slog.LevelInfo, FormatJSON)
	logger.Info("hello", "k", "v")

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("output not JSON: %v (%q)", err, buf.String())
	}
	if m["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", m["msg"])
	}
	if m["k"] != "v" {
		t.Errorf("k = %v, want v", m["k"])
	}
}

func TestNewFormatText(t *testing.T) {
	// No t.Parallel(): logfilter.New uses a process-wide default handler
	// (required for runtime filter mutation via the library's package-
	// level API). Parallel tests would race over that global — we keep
	// these serial instead.
	var buf bytes.Buffer
	logger := New(&buf, slog.LevelInfo, FormatText)
	logger.Info("hello", "k", "v")

	out := buf.String()
	if !strings.Contains(out, "msg=hello") || !strings.Contains(out, "k=v") {
		t.Errorf("text format missing fields: %q", out)
	}
}

func TestNewLevelFilter(t *testing.T) {
	// No t.Parallel(): logfilter.New uses a process-wide default handler
	// (required for runtime filter mutation via the library's package-
	// level API). Parallel tests would race over that global — we keep
	// these serial instead.
	var buf bytes.Buffer
	logger := New(&buf, slog.LevelWarn, FormatJSON)
	logger.Info("skipped")
	logger.Warn("kept")

	if strings.Contains(buf.String(), "skipped") {
		t.Errorf("info leaked past warn filter: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "kept") {
		t.Errorf("warn lost: %q", buf.String())
	}
}

func TestAutoFormatNonTTY(t *testing.T) {
	// No t.Parallel(): logfilter.New uses a process-wide default handler
	// (required for runtime filter mutation via the library's package-
	// level API). Parallel tests would race over that global — we keep
	// these serial instead.
	var buf bytes.Buffer
	logger := New(&buf, slog.LevelInfo, FormatAuto)
	logger.Info("hi")

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Errorf("auto on non-TTY should be JSON, got %q", buf.String())
	}
}

func TestWithComponent(t *testing.T) {
	// No t.Parallel(): logfilter.New uses a process-wide default handler
	// (required for runtime filter mutation via the library's package-
	// level API). Parallel tests would race over that global — we keep
	// these serial instead.
	var buf bytes.Buffer
	base := New(&buf, slog.LevelInfo, FormatJSON)
	child := WithComponent(base, "memory")
	child.Info("started")

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["component"] != "memory" {
		t.Errorf("component = %v, want memory", m["component"])
	}
}

func TestContextAttachment(t *testing.T) {
	// No t.Parallel(): logfilter.New uses a process-wide default handler
	// (required for runtime filter mutation via the library's package-
	// level API). Parallel tests would race over that global — we keep
	// these serial instead.
	var buf bytes.Buffer
	logger := New(&buf, slog.LevelInfo, FormatJSON)

	ctx := Into(context.Background(), logger)
	got := From(ctx)
	if got != logger {
		t.Errorf("From returned a different logger than was stored")
	}

	fallback := From(context.Background())
	if fallback == nil {
		t.Error("From should fall back to slog.Default, not nil")
	}
}
