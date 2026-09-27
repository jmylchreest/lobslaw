package compute

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type deadlineTransport func(*http.Request) (*http.Response, error)

func (f deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLLMClientDeadlinePolicy(t *testing.T) {
	t.Parallel()
	for _, timeout := range []time.Duration{0, 30 * time.Second} {
		for _, callerTimeout := range []time.Duration{0, time.Second, 5 * time.Minute} {
			t.Run(timeout.String()+"/"+callerTimeout.String(), func(t *testing.T) {
				t.Parallel()
				client, err := NewLLMClient(LLMClientConfig{Endpoint: "http://provider.test", Timeout: timeout})
				if err != nil {
					t.Fatal(err)
				}
				if client.httpClient.Timeout != 0 {
					t.Errorf("default HTTP timeout caps caller deadlines: %v", client.httpClient.Timeout)
				}
				ctx := context.Background()
				if callerTimeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, callerTimeout)
					defer cancel()
				}
				client.httpClient.Transport = deadlineTransport(func(r *http.Request) (*http.Response, error) {
					deadline, ok := r.Context().Deadline()
					if !ok {
						t.Error("request has no deadline")
					} else if parent, hasParent := ctx.Deadline(); hasParent {
						if !deadline.Equal(parent) {
							t.Errorf("deadline = %v, want caller deadline %v", deadline, parent)
						}
					} else {
						want := orDefault(timeout, DefaultLLMTimeout)
						if left := time.Until(deadline); left <= want-time.Second || left > want {
							t.Errorf("fallback deadline in %v, want %v", left, want)
						}
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`))}, nil
				})
				if _, err := client.Chat(ctx, ChatRequest{}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestLLMClientLongerDeadlineAllowsSlowResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(80 * time.Millisecond)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()
	client, err := NewLLMClient(LLMClientConfig{Endpoint: srv.URL, Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if resp, err := client.Chat(ctx, ChatRequest{}); err != nil || resp.Content != "ok" {
		t.Fatalf("longer caller deadline should allow the response: resp=%v err=%v", resp, err)
	}
}

func TestLLMClientFallbackBoundsResponseBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	client, err := NewLLMClient(LLMClientConfig{Endpoint: srv.URL, Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Chat(context.Background(), ChatRequest{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("body read should reach the fallback deadline: %v", err)
	}
}
