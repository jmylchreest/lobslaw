package embedder

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Proves the DOCUMENTED url shape works against the real host rather
// than a stub. The unit tests use a fake server, which validates the
// logic and cannot validate the thing most likely to be wrong: whether
//
//	https://huggingface.co/<org>/<repo>/resolve/main
//
// actually serves the file set Ensure requires.
//
// Opt-in, because it reaches the internet: set
// LOBSLAW_EMBEDDER_LIVE_FETCH=1.
func TestLiveFetchFromHuggingFace(t *testing.T) {
	if os.Getenv("LOBSLAW_EMBEDDER_LIVE_FETCH") == "" {
		t.Skip("set LOBSLAW_EMBEDDER_LIVE_FETCH=1")
	}
	dir := t.TempDir()
	// A tiny BERT so the test is seconds, not minutes — Ensure is
	// model-agnostic, so what is under test is the URL shape and the
	// file set, not this checkpoint.
	const base = "https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2/resolve/main"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	got, err := Ensure(ctx, &http.Client{Timeout: 4 * time.Minute}, dir, "all-MiniLM-L6-v2", base)
	if err != nil {
		t.Fatalf("Ensure against the documented URL shape: %v", err)
	}
	for _, f := range []string{"config.json", "model.safetensors", "tokenizer.json"} {
		st, err := os.Stat(filepath.Join(got, f))
		if err != nil {
			t.Errorf("%s missing: %v", f, err)
			continue
		}
		if st.Size() == 0 {
			t.Errorf("%s is empty", f)
		}
	}
}

// A repository with no safetensors must say WHY rather than reporting a
// bare 404.
//
// Many older HuggingFace repositories ship only pytorch_model.bin — a
// Python pickle, which is arbitrary code execution on load and is
// therefore refused rather than supported. "fetch model.safetensors:
// HTTP 404" reads as a broken URL and sends an operator to check their
// typing instead of their repository.
func TestAPickleOnlyRepositoryExplainsItself(t *testing.T) {
	if os.Getenv("LOBSLAW_EMBEDDER_LIVE_FETCH") == "" {
		t.Skip("set LOBSLAW_EMBEDDER_LIVE_FETCH=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := Ensure(ctx, &http.Client{Timeout: 30 * time.Second}, t.TempDir(),
		"bert-tiny", "https://huggingface.co/prajjwal1/bert-tiny/resolve/main")
	if err == nil {
		t.Fatal("a repository with no safetensors was accepted")
	}
	for _, want := range []string{"safetensors", "pytorch_model.bin", "pickle"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q:\n%v", want, err)
		}
	}
}
