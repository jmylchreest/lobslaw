package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The callback is the one delivery path that survives the caller
// disconnecting, which is the whole reason it exists: a generation runs
// for minutes and the request that asked for it is long closed.
func TestCallbackSinkPostsJSON(t *testing.T) {
	var gotBody, gotType, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := &CallbackSink{Client: srv.Client()}
	if err := s.Deliver(context.Background(), srv.URL, "your generation is ready"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotType != "application/json" {
		t.Errorf("content-type = %q", gotType)
	}
	var p callbackPayload
	if err := json.Unmarshal([]byte(gotBody), &p); err != nil {
		t.Fatalf("body is not JSON (%v): %s", err, gotBody)
	}
	if p.Body != "your generation is ready" {
		t.Errorf("body = %q", p.Body)
	}
}

// A receiver that rejects the callback must surface as an error, not a
// silent drop — a dropped notification is indistinguishable from work
// that never finished.
func TestCallbackSinkReportsReceiverFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := (&CallbackSink{Client: srv.Client()}).Deliver(context.Background(), srv.URL, "x")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("want the status reported, got %v", err)
	}
}

// The address arrives from a replicated user record, so the scheme is
// re-checked here rather than trusted from boot validation alone.
func TestCallbackSinkRejectsBadAddresses(t *testing.T) {
	for _, addr := range []string{"", "ops.example/hook", "ftp://ops.example", "://nope"} {
		t.Run(addr, func(t *testing.T) {
			err := (&CallbackSink{Client: http.DefaultClient}).Deliver(context.Background(), addr, "x")
			if err == nil {
				t.Errorf("address %q should be refused", addr)
			}
		})
	}
}

func TestCallbackSinkChannelTypeIsNotWebhook(t *testing.T) {
	// "webhook" is the INBOUND [[gateway.channels]] type. Sharing the
	// word would make every config question ambiguous.
	if got := (&CallbackSink{}).ChannelType(); got != "callback" {
		t.Errorf("ChannelType = %q, want %q", got, "callback")
	}
}

func TestCallbackSinkNeedsAClient(t *testing.T) {
	if err := (&CallbackSink{}).Deliver(context.Background(), "https://ops.example", "x"); err == nil {
		t.Error("want an error when no client is wired")
	}
}
