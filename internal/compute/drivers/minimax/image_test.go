package minimax

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

func cred() compute.Credential { return compute.NewHeaderCredential("Authorization", "Bearer k") }

// THE ONE THAT MATTERS. MiniMax reports failure as HTTP 200 with
// data:null and a status code in the body — an unsupported model
// comes back 200/2013. A driver trusting the status line reports
// "provider returned no images" and throws away the one sentence
// saying why.
func TestAFailureOnAn200IsReadFromTheBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"x","data":null,"base_resp":{"status_code":2013,"status_msg":"invalid params, unsupported model: no-such-model"}}`))
	}))
	defer srv.Close()

	d, err := NewImage(ImageConfig{Endpoint: srv.URL, Model: "m", Credential: cred()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Generate(context.Background(), compute.ImageRequest{Prompt: "p"})
	if err == nil {
		t.Fatal("a 200-with-error-body was treated as success")
	}
	if !strings.Contains(err.Error(), "unsupported model") {
		t.Errorf("the vendor's reason was dropped: %v", err)
	}
}

// A rejected model will be rejected again; a rate limit will not.
// Retrying the first burns quota to be told the same thing.
func TestOnlyWorthwhileFailuresAreRetryable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		code      int
		transient bool
	}{
		{2013, false}, // unsupported model
		{1002, true},  // rate limited
		{1039, true},  // concurrency limited
		{1013, true},  // internal error
	} {
		err := classifyStatus(baseResp{StatusCode: tc.code, StatusMsg: "x"})
		var de *compute.DriverError
		if !errors.As(err, &de) {
			t.Fatalf("status %d did not produce a DriverError", tc.code)
		}
		got := de.Class == compute.FailureTransient
		if got != tc.transient {
			t.Errorf("status %d transient = %v, want %v", tc.code, got, tc.transient)
		}
	}
}

func TestASuccessReturnsTheFirstURL(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"image_urls":["https://example.com/a.jpg"]},"base_resp":{"status_code":0,"status_msg":"success"}}`))
	}))
	defer srv.Close()

	d, _ := NewImage(ImageConfig{Endpoint: srv.URL, Model: "m", Credential: cred()})
	art, err := d.Generate(context.Background(), compute.ImageRequest{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if art.URL != "https://example.com/a.jpg" || art.Kind != compute.ArtifactURL {
		t.Errorf("artifact = %+v", art)
	}
}

// An empty prompt reaching the vendor costs a round trip to be told
// what we already knew.
func TestAnEmptyPromptIsRefusedLocally(t *testing.T) {
	t.Parallel()
	d, _ := NewImage(ImageConfig{Endpoint: "http://127.0.0.1:1", Model: "m", Credential: cred()})
	if _, err := d.Generate(context.Background(), compute.ImageRequest{Prompt: "  "}); err == nil {
		t.Error("an empty prompt was sent to the provider")
	}
}
