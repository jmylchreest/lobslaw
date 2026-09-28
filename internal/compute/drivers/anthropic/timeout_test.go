package anthropic

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

type deadlineTransport func(*http.Request) (*http.Response, error)

func (f deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAnthropicDeadlinePolicy(t *testing.T) {
	t.Parallel()
	for _, callerTimeout := range []time.Duration{0, time.Second, 5 * time.Minute} {
		t.Run(callerTimeout.String(), func(t *testing.T) {
			t.Parallel()
			d := newDriver(t, "http://provider.test")
			if d.client.Timeout != 0 {
				t.Errorf("default HTTP timeout caps caller deadlines: %v", d.client.Timeout)
			}
			ctx := context.Background()
			if callerTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, callerTimeout)
				defer cancel()
			}
			d.client.Transport = deadlineTransport(func(r *http.Request) (*http.Response, error) {
				deadline, ok := r.Context().Deadline()
				if !ok {
					t.Error("request has no deadline")
				} else if parent, hasParent := ctx.Deadline(); hasParent {
					if !deadline.Equal(parent) {
						t.Errorf("deadline = %v, want caller deadline %v", deadline, parent)
					}
				} else if left := time.Until(deadline); left <= compute.DefaultLLMTimeout-time.Second || left > compute.DefaultLLMTimeout {
					t.Errorf("fallback deadline in %v, want %v", left, compute.DefaultLLMTimeout)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(okResponse))}, nil
			})
			if _, err := d.Chat(ctx, compute.ChatRequest{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAnthropicDeadlineBoundsResponseBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	d := newDriver(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := d.Chat(ctx, compute.ChatRequest{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("body read should reach the caller deadline: %v", err)
	}
}
