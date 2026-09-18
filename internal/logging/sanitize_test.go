package logging

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"
)

func TestAllOutputSanitized(t *testing.T) {
	const token = "123456789:ABCDEFGHIJKLM_secret_token"
	const testValue = "hidden-value"
	for _, format := range []Format{FormatJSON, FormatText} {
		t.Run(string(format), func(t *testing.T) {
			var out bytes.Buffer
			l := New(&out, slog.LevelDebug, format)
			l.With("refresh_token", "hidden-value").WithGroup("request").Error("https://api.telegram.org/bot"+token+"/sendMessage", "error", &url.Error{Op: "Post", URL: "https://api.telegram.org/file/bot" + token + "/audio.ogg", Err: errors.New("dial failed")}, "data", map[string]any{"password": testValue, "count": 2})
			for _, secret := range []string{token, "hidden-value"} {
				if strings.Contains(out.String(), secret) {
					t.Fatal(out.String())
				}
			}
			if !strings.Contains(out.String(), "dial failed") {
				t.Fatal("lost diagnosis")
			}
		})
	}
}
func TestSafeErrorRetainsCause(t *testing.T) {
	original := &url.Error{Op: "Get", URL: "https://api.telegram.org/bot123456789:ABCDEFGHIJKLM_secret_token/getUpdates", Err: errors.New("offline")}
	safe := SafeError(original)
	var got *url.Error
	if !errors.As(safe, &got) || got != original {
		t.Fatal("lost URL cause")
	}
	for _, s := range []string{safe.Error(), fmt.Sprintf("%+v", safe), fmt.Sprintf("%#v", safe)} {
		if strings.Contains(s, "ABCDEFGHIJKLM_secret_token") {
			t.Fatal(s)
		}
	}
}

func TestStandardDiagnosticsSanitized(t *testing.T) {
	var out bytes.Buffer
	l := New(&out, slog.LevelInfo, FormatJSON)
	StandardLogger(l, slog.LevelError).Print("Bearer secret-value")
	_, _ = Writer(l, slog.LevelWarn).Write([]byte("password=secret-value"))
	if out.Len() == 0 || strings.Contains(out.String(), "secret-value") {
		t.Fatal(out.String())
	}
}
