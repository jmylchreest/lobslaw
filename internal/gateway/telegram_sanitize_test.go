package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/logging"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type failedTelegramTransport struct {
	cause error
	body  bool
}

func (f failedTelegramTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if f.body {
		if strings.HasSuffix(req.URL.Path, "/getFile") {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"file_path":"audio.ogg"}}`)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("failed " + req.URL.String())), Header: make(http.Header)}, nil
	}
	return nil, f.cause
}
func TestTelegramErrorsAndLogsHideToken(t *testing.T) {
	const token = "123456789:ABCDEFGHIJKLM_secret_token"
	cause := errors.New("dial failed")
	for _, body := range []bool{false, true} {
		t.Run(fmt.Sprint(body), func(t *testing.T) {
			var out bytes.Buffer
			h := &TelegramHandler{base: "https://api.telegram.org", cfg: TelegramConfig{BotToken: token}, client: &http.Client{Transport: failedTelegramTransport{cause: cause, body: body}}, log: logging.New(&out, slog.LevelDebug, logging.FormatJSON), pollSlack: time.Second}
			calls := []struct {
				name string
				run  func() error
			}{
				{"send", func() error { return h.Send(1, "hello") }},
				{"updates", func() error { _, _, e := h.getUpdates(context.Background(), 0, time.Second); return e }},
				{"delete", func() error { return h.deleteWebhook(context.Background()) }},
				{"file", func() error { _, e := h.resolveFileURL(context.Background(), "id"); return e }},
				{"download", func() error {
					_, e := h.downloadOne(context.Background(), t.TempDir(), &types.Attachment{Reference: "id"})
					return e
				}},
				{"attachment", func() error {
					return h.sendAttachment(1, types.Attachment{Kind: types.AttachmentVoice, Reference: "audio"}, func(string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("audio")), nil })
				}},
			}
			for _, call := range calls {
				t.Run(call.name, func(t *testing.T) {
					e := call.run()
					if body && call.name == "file" {
						if e != nil {
							t.Fatalf("successful getFile: %v", e)
						}
						return
					}
					if e == nil {
						t.Fatal("expected failure")
					}
					for _, text := range []string{e.Error(), fmt.Sprintf("%+v", e), fmt.Sprintf("%#v", e)} {
						if strings.Contains(text, token) {
							t.Fatal(text)
						}
					}
					if !body {
						var ue *url.Error
						if !errors.Is(e, cause) || !errors.As(e, &ue) {
							t.Fatal("lost transport cause", e)
						}
					}
				})
			}
			h.sendText(1, "hello")
			h.postJSON("sendMessage", map[string]string{"text": "hello"})
			if strings.Contains(out.String(), token) {
				t.Fatal(out.String())
			}
		})
	}
}
