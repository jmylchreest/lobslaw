package embedder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeHub(t *testing.T, files map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

var minimalModel = map[string]string{
	"config.json":           `{"model_type":"bert","hidden_size":8}`,
	"model.safetensors":     "not-really-a-model",
	"tokenizer.json":        `{}`,
	"1_Pooling/config.json": `{"pooling_mode_mean_tokens":true}`,
}

func TestEnsureDownloadsAndCaches(t *testing.T) {
	t.Parallel()
	srv := fakeHub(t, minimalModel)
	dir := t.TempDir()

	got, err := Ensure(context.Background(), srv.Client(), dir, "e5-base", srv.URL)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, f := range []string{"config.json", "model.safetensors", "tokenizer.json"} {
		if _, err := os.Stat(filepath.Join(got, f)); err != nil {
			t.Errorf("%s was not written: %v", f, err)
		}
	}
	// Nested paths must be created, not flattened or dropped.
	if _, err := os.Stat(filepath.Join(got, "1_Pooling", "config.json")); err != nil {
		t.Errorf("the pooling declaration was not fetched: %v", err)
	}
	// A checksum is recorded so a node that behaves oddly later can be
	// asked what it actually downloaded.
	if _, err := os.Stat(filepath.Join(got, "model.safetensors.sha256")); err != nil {
		t.Errorf("no checksum recorded: %v", err)
	}
}

// A second call must not re-download 1.1 GB.
func TestEnsureIsIdempotent(t *testing.T) {
	t.Parallel()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		body, ok := minimalModel[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	dir := t.TempDir()

	if _, err := Ensure(context.Background(), srv.Client(), dir, "m", srv.URL); err != nil {
		t.Fatal(err)
	}
	first := hits
	if _, err := Ensure(context.Background(), srv.Client(), dir, "m", srv.URL); err != nil {
		t.Fatal(err)
	}
	if hits != first {
		t.Errorf("second Ensure made %d more requests; a cached model was re-downloaded", hits-first)
	}
}

// AIR-GAPPED IS THE DEFAULT. With no download_url, a missing model is
// an error at boot rather than a silent fetch.
func TestEnsureWithoutADownloadURLNeverFetches(t *testing.T) {
	t.Parallel()
	_, err := Ensure(context.Background(), http.DefaultClient, t.TempDir(), "absent", "")
	if err == nil {
		t.Fatal("a missing model with no download_url was accepted")
	}
	if !strings.Contains(err.Error(), "download_url") {
		t.Errorf("the error does not say what to set: %v", err)
	}
}

// The model name becomes a filesystem path. Configuration is not the
// same as trust: a name that escapes the models directory would turn
// "point at a model" into "write anywhere the node can".
func TestEnsureRefusesAPathEscape(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"../escape", "a/b", "..", "./x", "/abs"} {
		if _, err := Ensure(context.Background(), nil, t.TempDir(), bad, "http://example"); err == nil {
			t.Errorf("model name %q was accepted", bad)
		}
	}
}

// An interrupted download must not leave something that loads as a
// valid model next boot.
func TestAPartialDownloadIsNotLeftInPlace(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "model.safetensors") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		body := minimalModel[strings.TrimPrefix(r.URL.Path, "/")]
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	dir := t.TempDir()

	if _, err := Ensure(context.Background(), srv.Client(), dir, "m", srv.URL); err == nil {
		t.Fatal("a failed download reported success")
	}
	if _, err := os.Stat(filepath.Join(ModelPath(dir, "m"), "model.safetensors")); err == nil {
		t.Error("a failed download left model.safetensors in place")
	}
	// And no stray temporaries.
	entries, _ := os.ReadDir(ModelPath(dir, "m"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".partial-") {
			t.Errorf("left a temporary file behind: %s", e.Name())
		}
	}
}

func TestEnsureRequiresAModelName(t *testing.T) {
	t.Parallel()
	if _, err := Ensure(context.Background(), nil, t.TempDir(), "", "http://x"); err == nil {
		t.Error("an empty model name was accepted")
	}
}

// A mirror that cannot express "1_Pooling/config.json" — GitHub release
// assets may not contain "/" — must still deliver the pooling
// declaration under a flattened name, and it must land on disk at the
// nested path so what results is an ordinary snapshot.
//
// Without this the file is merely absent and pooling silently falls
// back to the family default. That default happens to be right for
// all-MiniLM, which is precisely why it needed a test: a mirror of some
// future CLS-pooled model would load, run, and be quietly wrong.
func TestAFlattenedPoolingFileIsAccepted(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"config.json":           `{"model_type":"bert","hidden_size":8}`,
		"model.safetensors":     "x",
		"tokenizer.json":        `{}`,
		"1_Pooling.config.json": `{"pooling_mode_cls_token":true}`,
	}
	srv := fakeHub(t, files)
	dir := t.TempDir()

	got, err := Ensure(context.Background(), srv.Client(), dir, "m", srv.URL)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(got, "1_Pooling", "config.json"))
	if err != nil {
		t.Fatalf("the flattened pooling file was not stored at the nested path: %v", err)
	}
	if !strings.Contains(string(raw), "pooling_mode_cls_token") {
		t.Errorf("wrong content: %s", raw)
	}
	// And the loader must now read CLS from it rather than defaulting.
	if p := poolingConfig(got, PoolMean); p != PoolCLS {
		t.Errorf("pooling = %q, want cls — the declaration was not honoured", p)
	}
}
