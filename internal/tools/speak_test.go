package tools

import (
	"context"
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

func speakBuiltin(t *testing.T, srvURL string, maxChars int) (compute.BuiltinFunc, string) {
	t.Helper()
	root := t.TempDir()
	b := NewBuiltins()
	if err := RegisterSpeakBuiltin(b, SpeakConfig{
		Driver:   newSpeakDriver(t, srvURL),
		Resolver: &compute.ArtifactResolver{Mounts: fakeMounts{label: "store", root: root}, DefaultMount: "store"},
		MaxChars: maxChars,
	}); err != nil {
		t.Fatal(err)
	}
	h, ok := b.Get("speak")
	if !ok {
		t.Fatal("speak not registered")
	}
	return h, root
}

// What the model gets back is a PATH, not audio. Bytes in a tool
// result would be unreadable to the model and enormous in context.
func TestSpeakBuiltinReturnsAPath(t *testing.T) {
	t.Parallel()
	srv := speakServer(t, http.StatusOK, []byte("ID3-fake"), nil)
	h, root := speakBuiltin(t, srv.URL, 0)

	out, code, err := h(context.Background(), map[string]string{"text": "Hello there world"})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	var got struct{ Mount, Path, MIME string }
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result is not JSON: %v (%s)", err, out)
	}
	if got.Mount != "store" {
		t.Errorf("mount = %q, want store", got.Mount)
	}
	if !strings.HasSuffix(got.Path, ".mp3") {
		t.Errorf("path %q has no audio extension", got.Path)
	}
	// A readable name makes a mount of generated audio browsable.
	if !strings.Contains(got.Path, "hello") {
		t.Errorf("path %q does not reflect the spoken text", got.Path)
	}
	if _, err := os.Stat(filepath.Join(root, got.Path)); err != nil {
		t.Errorf("audio not written: %v", err)
	}
	if strings.Contains(string(out), "ID3-fake") {
		t.Error("the audio bytes were returned to the model instead of a path")
	}
}

// TTS is billed per character, so an over-long passage is refused
// with an argument error the model can act on rather than truncated
// into audio that stops mid-sentence.
func TestSpeakBuiltinRefusesOverlongText(t *testing.T) {
	t.Parallel()
	srv := speakServer(t, http.StatusOK, []byte("audio"), nil)
	h, _ := speakBuiltin(t, srv.URL, 10)

	_, code, err := h(context.Background(), map[string]string{"text": strings.Repeat("a", 50)})
	if err == nil {
		t.Fatal("an over-long passage was synthesised anyway")
	}
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (bad argument the model can fix)", code)
	}
	if !strings.Contains(err.Error(), "limit is 10") {
		t.Errorf("error should state the limit, got: %v", err)
	}
}

func TestSpeakBuiltinRequiresText(t *testing.T) {
	t.Parallel()
	srv := speakServer(t, http.StatusOK, []byte("audio"), nil)
	h, _ := speakBuiltin(t, srv.URL, 0)
	if _, code, err := h(context.Background(), map[string]string{}); err == nil || code != 2 {
		t.Errorf("missing text: code=%d err=%v, want an argument error", code, err)
	}
}

// A speak tool with nowhere to write would bill for synthesis and
// then drop the result, so construction fails rather than registering
// a tool that cannot succeed.
func TestSpeakBuiltinRequiresAResolver(t *testing.T) {
	t.Parallel()
	srv := speakServer(t, http.StatusOK, []byte("audio"), nil)
	if err := RegisterSpeakBuiltin(NewBuiltins(), SpeakConfig{
		Driver: newSpeakDriver(t, srv.URL),
	}); err == nil {
		t.Error("registered a speak tool with no artifact resolver")
	}
}

func speakServer(t *testing.T, status int, body []byte, record any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if record != nil {
			_ = json.NewDecoder(r.Body).Decode(record)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}
