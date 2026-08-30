package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// --- the builtin -----------------------------------------------------

// The image must land on disk and be announced for the channel to
// attach, exactly as speech is. Returning bytes to the model would be
// unreadable and enormous.
func TestImageBuiltinWritesAndAnnounces(t *testing.T) {
	t.Parallel()
	b64 := base64.StdEncoding.EncodeToString([]byte("PNGDATA"))
	srv := imageServer(t, http.StatusOK, `{"data":[{"b64_json":"`+b64+`"}]}`, nil)

	root := t.TempDir()
	b := NewBuiltins()
	if err := RegisterImageBuiltin(b, ImageConfig{
		Driver:   newImageDriver(t, srv.URL, false),
		Resolver: &compute.ArtifactResolver{Mounts: fakeMounts{label: "store", root: root}, DefaultMount: "store"},
	}); err != nil {
		t.Fatal(err)
	}
	h, ok := b.Get("generate_image")
	if !ok {
		t.Fatal("generate_image not registered")
	}

	ctx, collector := compute.WithArtifactCollector(context.Background())
	out, code, err := h(ctx, map[string]string{"prompt": "A red cube on grass"})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}

	var got struct{ Mount, Path, MIME string }
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result is not JSON: %v (%s)", err, out)
	}
	if !strings.HasSuffix(got.Path, ".png") {
		t.Errorf("path %q has no image extension", got.Path)
	}
	if !strings.Contains(got.Path, "red") {
		t.Errorf("path %q does not reflect the prompt", got.Path)
	}
	if _, err := os.Stat(filepath.Join(root, got.Path)); err != nil {
		t.Errorf("image not written: %v", err)
	}

	// Announced, and as an image — the channel switches on the kind to
	// pick sendPhoto over sendDocument.
	atts := collector.Collected()
	if len(atts) != 1 {
		t.Fatalf("announced %d artifacts, want 1 — the channel has nothing to attach", len(atts))
	}
	if atts[0].Kind != "image" {
		t.Errorf("kind = %q, want image", atts[0].Kind)
	}
	if atts[0].Reference != got.Mount+":"+got.Path {
		t.Errorf("reference %q does not match the written path", atts[0].Reference)
	}
}

func TestImageBuiltinRequiresPromptAndResolver(t *testing.T) {
	t.Parallel()
	srv := imageServer(t, http.StatusOK, `{"data":[{"b64_json":"eA=="}]}`, nil)

	if err := RegisterImageBuiltin(NewBuiltins(), ImageConfig{
		Driver: newImageDriver(t, srv.URL, false),
	}); err == nil {
		t.Error("registered generate_image with no resolver")
	}

	b := NewBuiltins()
	if err := RegisterImageBuiltin(b, ImageConfig{
		Driver:   newImageDriver(t, srv.URL, false),
		Resolver: &compute.ArtifactResolver{Mounts: fakeMounts{label: "store", root: t.TempDir()}, DefaultMount: "store"},
	}); err != nil {
		t.Fatal(err)
	}
	h, _ := b.Get("generate_image")
	if _, code, err := h(context.Background(), map[string]string{}); err == nil || code != 2 {
		t.Errorf("missing prompt: code=%d err=%v, want an argument error", code, err)
	}
}

func imageServer(t *testing.T, status int, body string, record any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if record != nil {
			_ = json.NewDecoder(r.Body).Decode(record)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}
