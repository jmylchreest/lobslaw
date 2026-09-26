package compute

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestTruncateBodyRedactsBeforeCut(t *testing.T) {
	token := "123456789:" + strings.Repeat("a", 600)
	got := truncateBody([]byte("https://api.telegram.org/bot" + token + "/sendMessage"))
	if strings.Contains(got, "123456789") || strings.Contains(got, strings.Repeat("a", 20)) {
		t.Fatal(got)
	}
}

type errorBodyTransport struct{ body *countedErrorBody }

func (t errorBodyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 429, Body: t.body, Header: make(http.Header)}, nil
}

type countedErrorBody struct {
	io.Reader
	count  int
	closed bool
}

func (b *countedErrorBody) Read(p []byte) (int, error) {
	n, e := b.Reader.Read(p)
	b.count += n
	return n, e
}
func (b *countedErrorBody) Close() error { b.closed = true; return nil }
func TestLLMErrorBodyReadIsBounded(t *testing.T) {
	body := &countedErrorBody{Reader: strings.NewReader(`{"token":"` + strings.Repeat("private", 1<<17) + `"}`)}
	var logs bytes.Buffer
	c, err := NewLLMClient(LLMClientConfig{Endpoint: "http://test", Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	c.httpClient = &http.Client{Transport: errorBodyTransport{body: body}}
	c.log = slog.New(slog.NewTextHandler(&logs, nil))
	_, err = c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "test"}}})
	if !errors.Is(err, ErrLLMRateLimit) {
		t.Fatal(err)
	}
	if body.count > (64<<10)+1 || !body.closed {
		t.Fatalf("read=%d closed=%v", body.count, body.closed)
	}
	if strings.Contains(err.Error(), "private") || strings.Contains(logs.String(), "private") {
		t.Fatal("partial credential escaped")
	}
}
