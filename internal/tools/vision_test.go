package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// The Anthropic and Gemini wire-shape tests moved to their driver
// packages when read_image stopped switching on a format enum. Each
// driver tests its own bytes now, which is the point of the seam.
//
// What stays here is the BUILTIN's business: the path check, the MIME
// sniff, and handing off to whichever driver it was given.

// openAIVisionFor builds the default-shaped driver against a test
// server.
func openAIVisionFor(t *testing.T, endpoint string) compute.VisionDriver {
	t.Helper()
	d, err := compute.OpenAIVisionFactory(compute.VisionDriverConfig{
		Endpoint:   endpoint,
		Model:      "test-vl",
		Credential: compute.NewBearerCredential("fake"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestReadImageDispatchesToVisionEndpoint(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	imgPath := filepath.Join(tmp, "shot.jpg")
	if err := os.WriteFile(imgPath,
		[]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}, 0o600); err != nil {
		t.Fatal(err)
	}

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"a token plan screenshot"}}]}`))
	}))
	t.Cleanup(srv.Close)

	b := NewBuiltins()
	if err := RegisterVisionBuiltin(b, VisionConfig{
		Endpoint:    srv.URL,
		Model:       "test-vl",
		APIKey:      "fake",
		AllowedRoot: tmp,
		Driver:      openAIVisionFor(t, srv.URL),
	}); err != nil {
		t.Fatalf("RegisterVisionBuiltin: %v", err)
	}

	fn, ok := b.Get("read_image")
	if !ok {
		t.Fatal("read_image not registered")
	}
	out, code, err := fn(context.Background(), map[string]string{
		"path":     imgPath,
		"question": "what's here?",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}

	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if got := resp["content"]; got != "a token plan screenshot" {
		t.Errorf("content = %q, want screenshot summary", got)
	}

	if !strings.Contains(string(gotBody), `"image_url"`) {
		t.Errorf("expected multimodal request body to include image_url; got %s", gotBody)
	}
	// The MIME sniff is the builtin's, and the driver renders it into
	// the data URL. A sniff that guessed wrong here would send a JPEG
	// labelled as something else.
	if !strings.Contains(string(gotBody), `"data:image/jpeg;base64,`) {
		t.Errorf("expected base64 data URL; got %s", gotBody)
	}
}

func TestReadImageRefusesPathOutsideAllowedRoot(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("vision endpoint should not be called when path scope check fails")
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)

	b := NewBuiltins()
	if err := RegisterVisionBuiltin(b, VisionConfig{
		Endpoint:    srv.URL,
		Model:       "test-vl",
		APIKey:      "fake",
		AllowedRoot: "/workspace/incoming",
		Driver:      openAIVisionFor(t, srv.URL),
	}); err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	out := filepath.Join(tmp, "evil.jpg")
	_ = os.WriteFile(out, []byte("\xff\xd8"), 0o600)

	fn, ok := b.Get("read_image")
	if !ok {
		t.Fatal("read_image not registered")
	}
	_, code, err := fn(context.Background(), map[string]string{"path": out})
	if err == nil {
		t.Error("expected error for path outside allowed root")
	}
	if code != 2 {
		t.Errorf("code = %d, want 2 (user-fixable)", code)
	}
}

func TestReadImageRequiresEndpointAndKey(t *testing.T) {
	t.Parallel()
	b := NewBuiltins()
	d := openAIVisionFor(t, "http://example.invalid")
	if err := RegisterVisionBuiltin(b, VisionConfig{
		Endpoint: "", APIKey: "x", Model: "m", Driver: d,
	}); err == nil {
		t.Error("expected error when Endpoint missing")
	}
	if err := RegisterVisionBuiltin(b, VisionConfig{
		Endpoint: "http://x", APIKey: "", Model: "m", Driver: d,
	}); err == nil {
		t.Error("expected error when APIKey missing")
	}
	if err := RegisterVisionBuiltin(b, VisionConfig{
		Endpoint: "http://x", APIKey: "k", Model: "", Driver: d,
	}); err == nil {
		t.Error("expected error when Model missing")
	}
}

// A config without a driver cannot serve anything, and the failure
// belongs at boot rather than the first time somebody sends a
// photograph.
func TestReadImageRequiresADriver(t *testing.T) {
	t.Parallel()
	b := NewBuiltins()
	err := RegisterVisionBuiltin(b, VisionConfig{
		Endpoint: "http://x", APIKey: "k", Model: "m",
	})
	if err == nil {
		t.Fatal("a config with no driver was accepted")
	}
	if !strings.Contains(err.Error(), "Driver") {
		t.Errorf("err = %q; it does not say what is missing", err)
	}
}
