package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTelegramResponseLimits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body string
		limit      int
	}{
		{"updates", `{"ok":true,"result":[{"update_id":10,"message":{"photo":[{"file_id":"image","file_size":15000000}],"voice":{"file_id":"voice","file_size":20000000}}}]}`, int(telegramPollMaxBytes)},
		{"file", `{"ok":true,"result":{"file_path":"voice.ogg"}}`, int(telegramAPIResponseMaxBytes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, extra := range []int{0, 1} {
				t.Run(string(rune('0'+extra)), func(t *testing.T) {
					body := tc.body + strings.Repeat(" ", tc.limit+extra-len(tc.body))
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.WriteHeader(200)
						w.(http.Flusher).Flush()
						_, _ = io.WriteString(w, body)
					}))
					defer srv.Close()
					h := &TelegramHandler{base: srv.URL, client: srv.Client(), pollSlack: time.Second}
					var err error
					if tc.name == "updates" {
						_, _, err = h.getUpdates(context.Background(), 0, time.Second)
					} else {
						_, err = h.resolveFileURL(context.Background(), "file")
					}
					if extra == 0 && err != nil {
						t.Fatal(err)
					}
					if extra == 1 && err == nil {
						t.Fatal("accepted oversized response")
					}
				})
			}
		})
	}
}

func TestTelegramErrorLogsBounded(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, strings.Repeat("x", 2048)+"DO_NOT_LOG")
	}))
	defer srv.Close()
	for _, method := range []string{"postJSON", "sendText"} {
		t.Run(method, func(t *testing.T) {
			var log strings.Builder
			h := &TelegramHandler{base: srv.URL, client: srv.Client(), log: slog.New(slog.NewTextHandler(&log, nil))}
			if method == "postJSON" {
				h.postJSON("sendMessage", map[string]string{"text": "hi"})
			} else {
				h.sendText(1, "hi")
			}
			if strings.Contains(log.String(), "DO_NOT_LOG") || strings.Contains(log.String(), strings.Repeat("x", 1025)) {
				t.Fatal("unbounded diagnostic logged")
			}
		})
	}
}
