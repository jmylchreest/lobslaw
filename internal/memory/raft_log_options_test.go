package memory

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"

	"github.com/jmylchreest/lobslaw/internal/logging"
)

func TestRaftStandardLogOptions(t *testing.T) {
	for _, tc := range []struct {
		name, input, level, msg string
		opts                    hclog.StandardLoggerOptions
	}{
		{"default", "[WARN] message", "INFO", "[WARN] message", hclog.StandardLoggerOptions{}},
		{"infer", "[WARN] message", "WARN", "message", hclog.StandardLoggerOptions{InferLevels: true}},
		{"timestamp", "2026/09/19 10:12:13 [ERROR] message", "ERROR", "message", hclog.StandardLoggerOptions{InferLevels: true, InferLevelsWithTimestamp: true}},
		{"force", "[DEBUG] message", "ERROR", "message", hclog.StandardLoggerOptions{InferLevels: true, ForceLevel: hclog.Error}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			h := newHCLogAdapter(logging.New(&out, slog.LevelDebug, logging.FormatJSON), "raft")
			h.StandardLogger(&tc.opts).Print(tc.input)
			var got struct{ Level, Msg string }
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Level != tc.level || got.Msg != tc.msg {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestRaftNilLoggerRedactsBeforeDefaultSetup(t *testing.T) {
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	defer slog.SetDefault(previous)
	newHCLogAdapter(nil, "raft").Warn("hello", "password", "secret-value")
	if out.Len() == 0 || strings.Contains(out.String(), "secret-value") {
		t.Fatal(out.String())
	}
}
