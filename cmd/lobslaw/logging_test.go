package main

import (
	"bytes"
	"flag"
	"log/slog"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/logging"
)

func TestCLIDiagnosticsSanitized(t *testing.T) {
	old := slog.Default()
	defer slog.SetDefault(old)
	var out bytes.Buffer
	slog.SetDefault(logging.New(&out, slog.LevelInfo, logging.FormatJSON))
	diagnosticf("request failed: %s", "https://api.telegram.org/bot123456789:ABCDEFGHIJKLM_secret_token/getUpdates")
	fs := newFlagSet("test", flag.ContinueOnError)
	fs.Int("count", 0, "count")
	_ = fs.Parse([]string{"--count=Bearer secret-value"})
	for _, secret := range []string{"ABCDEFGHIJKLM_secret_token", "secret-value"} {
		if strings.Contains(out.String(), secret) {
			t.Fatal(out.String())
		}
	}
	if out.Len() == 0 {
		t.Fatal("diagnostics lost")
	}
}

func TestFlagHelpRemainsCommandOutput(t *testing.T) {
	old := slog.Default()
	defer slog.SetDefault(old)
	var logs, help bytes.Buffer
	slog.SetDefault(logging.New(&logs, slog.LevelInfo, logging.FormatJSON))
	fs := flagSetWithHelpOutput("example", flag.ContinueOnError, &help)
	fs.String("name", "", "display name")
	if err := fs.Parse([]string{"--help"}); err != flag.ErrHelp {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Fatalf("help became a diagnostic: %s", logs.String())
	}
	if !strings.Contains(help.String(), "Usage of example:") || !strings.Contains(help.String(), "display name") {
		t.Fatal(help.String())
	}
}
