package dashscope

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

func cred() compute.Credential { return compute.NewHeaderCredential("Authorization", "Bearer k") }

// Every other vendor spells it "1024x1024". This one rejects that
// with HTTP 400 "expected format: width*height" — on every request
// carrying a size, and the operator cannot fix it from config because
// the size comes from the tool call.
func TestTheSizeIsTranslatedToTheVendorSpelling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ given, want string }{
		{"1024x1024", "1024*1024"},
		{"1792x1024", "1792*1024"},
		{"", ""},
		// Not WIDTHxHEIGHT: passed through. A vendor keyword this
		// driver has not heard of is the caller's business, and
		// mangling it is worse than forwarding it.
		{"square_hd", "square_hd"},
		{"1024*1024", "1024*1024"},
	} {
		if got := dashscopeSize(tc.given); got != tc.want {
			t.Errorf("dashscopeSize(%q) = %q, want %q", tc.given, got, tc.want)
		}
	}
}

// The request is a chat-shaped envelope even though nothing about it
// is a conversation. Sending {input:{prompt:...}} returns "url error,
// please check url", which names neither the field nor the problem.
func TestTheRequestUsesTheMessagesEnvelope(t *testing.T) {
	t.Parallel()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		// No async header may be sent: a token plan that forbids
		// asynchronous calls answers one with 403 AccessDenied.
		if r.Header.Get("X-DashScope-Async") != "" {
			t.Error("the async header was sent; plans that forbid async answer 403")
		}
		_, _ = w.Write([]byte(`{"output":{"choices":[{"message":{"content":[{"type":"image","image":"https://example.com/a.png"}]}}]}}`))
	}))
	defer srv.Close()

	d, err := NewImage(ImageConfig{Endpoint: srv.URL, Model: "wan2.7-image", Credential: cred()})
	if err != nil {
		t.Fatal(err)
	}
	art, err := d.Generate(context.Background(), compute.ImageRequest{Prompt: "a lobster"})
	if err != nil {
		t.Fatal(err)
	}
	if art.URL != "https://example.com/a.png" {
		t.Errorf("artifact url = %q", art.URL)
	}
	input, _ := got["input"].(map[string]any)
	if _, ok := input["messages"]; !ok {
		t.Errorf("request had no input.messages: %v", got)
	}
}

// A text part sits alongside the image when the model narrates what
// it drew. Taking content[0] blindly returns prose as a picture on
// exactly those replies.
func TestTheImagePartIsFoundPastANarratingTextPart(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"output":{"choices":[{"message":{"content":[
			{"text":"Here is the lobster you asked for."},
			{"type":"image","image":"https://example.com/real.png"}]}}]}}`))
	}))
	defer srv.Close()

	d, _ := NewImage(ImageConfig{Endpoint: srv.URL, Model: "m", Credential: cred()})
	art, err := d.Generate(context.Background(), compute.ImageRequest{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if art.URL != "https://example.com/real.png" {
		t.Errorf("artifact url = %q; a text part was taken as the image", art.URL)
	}
}

// A code on a 2xx is still a failure, and the vendor's wording is the
// only part of it that helps.
func TestAnErrorCodeOnA200IsAFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"InvalidParameter","message":"url error, please check url"}`))
	}))
	defer srv.Close()

	d, _ := NewImage(ImageConfig{Endpoint: srv.URL, Model: "m", Credential: cred()})
	_, err := d.Generate(context.Background(), compute.ImageRequest{Prompt: "p"})
	if err == nil {
		t.Fatal("an error code on a 200 was treated as success")
	}
	if !strings.Contains(err.Error(), "url error") {
		t.Errorf("the vendor's message was dropped: %v", err)
	}
}
