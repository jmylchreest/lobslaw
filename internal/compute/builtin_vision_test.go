package compute

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
)

func TestReadImageDispatchesToVisionEndpoint(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	imgPath := filepath.Join(tmp, "shot.jpg")
	if err := os.WriteFile(imgPath, []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}, 0o600); err != nil {
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
	_, code, err := fn(context.Background(), map[string]string{
		"path": out,
	})
	if err == nil {
		t.Error("expected error for path outside allowed root")
	}
	if code != 2 {
		t.Errorf("code = %d, want 2 (user-fixable)", code)
	}
}

func TestReadImageAnthropicFormat(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	imgPath := filepath.Join(tmp, "x.png")
	_ = os.WriteFile(imgPath, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}, 0o600)

	var gotBody []byte
	var gotKey, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"a tiny png"}]}`))
	}))
	t.Cleanup(srv.Close)

	b := NewBuiltins()
	if err := RegisterVisionBuiltin(b, VisionConfig{
		Endpoint:    srv.URL,
		Model:       "claude-opus-4",
		APIKey:      "sk-ant-x",
		Format:      VisionFormatAnthropic,
		AllowedRoot: tmp,
	}); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("read_image")
	out, code, err := fn(context.Background(), map[string]string{"path": imgPath})
	if err != nil || code != 0 {
		t.Fatalf("Dispatch: code=%d err=%v", code, err)
	}
	var resp map[string]any
	_ = json.Unmarshal(out, &resp)
	if got := resp["content"]; got != "a tiny png" {
		t.Errorf("content = %q, want anthropic content text", got)
	}
	if !strings.Contains(string(gotBody), `"source"`) || !strings.Contains(string(gotBody), `"media_type":"image/png"`) {
		t.Errorf("expected anthropic image source shape; got %s", gotBody)
	}
	if gotKey != "sk-ant-x" || gotVersion != "2023-06-01" {
		t.Errorf("missing anthropic auth/version headers (key=%q version=%q)", gotKey, gotVersion)
	}
}

func TestReadImageGeminiFormat(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	imgPath := filepath.Join(tmp, "x.jpg")
	_ = os.WriteFile(imgPath, []byte{0xFF, 0xD8, 0xFF}, 0o600)

	var gotBody []byte
	var gotKeyParam string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotKeyParam = r.URL.Query().Get("key")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"image looks like a jpeg header"}]}}]}`))
	}))
	t.Cleanup(srv.Close)

	b := NewBuiltins()
	if err := RegisterVisionBuiltin(b, VisionConfig{
		Endpoint:    srv.URL,
		Model:       "gemini-2.0-flash",
		APIKey:      "g-key",
		Format:      VisionFormatGemini,
		AllowedRoot: tmp,
	}); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("read_image")
	out, code, err := fn(context.Background(), map[string]string{"path": imgPath})
	if err != nil || code != 0 {
		t.Fatalf("Dispatch: code=%d err=%v", code, err)
	}
	var resp map[string]any
	_ = json.Unmarshal(out, &resp)
	if got := resp["content"]; got != "image looks like a jpeg header" {
		t.Errorf("content = %q", got)
	}
	if gotKeyParam != "g-key" {
		t.Errorf("expected gemini key as ?key= param; got %q", gotKeyParam)
	}
	if !strings.Contains(string(gotBody), `"inlineData"`) {
		t.Errorf("expected gemini inlineData shape; got %s", gotBody)
	}
}

func TestReadImageDefaultsToOpenAIFormat(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	imgPath := filepath.Join(tmp, "x.jpg")
	_ = os.WriteFile(imgPath, []byte{0xFF, 0xD8}, 0o600)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// OpenAI format = Bearer auth, not x-api-key.
		if r.Header.Get("Authorization") == "" {
			t.Errorf("expected Bearer auth (openai default); headers = %v", r.Header)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(srv.Close)

	b := NewBuiltins()
	if err := RegisterVisionBuiltin(b, VisionConfig{
		Endpoint:    srv.URL,
		Model:       "any",
		APIKey:      "k",
		AllowedRoot: tmp,
		// Format intentionally unset → must default to openai.
	}); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("read_image")
	if _, code, err := fn(context.Background(), map[string]string{"path": imgPath}); err != nil || code != 0 {
		t.Fatalf("default-format dispatch failed: code=%d err=%v", code, err)
	}
}

func TestReadImageRequiresEndpointAndKey(t *testing.T) {
	t.Parallel()
	b := NewBuiltins()
	if err := RegisterVisionBuiltin(b, VisionConfig{Endpoint: "", APIKey: "x", Model: "m"}); err == nil {
		t.Error("expected error when Endpoint missing")
	}
	if err := RegisterVisionBuiltin(b, VisionConfig{Endpoint: "http://x", APIKey: "", Model: "m"}); err == nil {
		t.Error("expected error when APIKey missing")
	}
	if err := RegisterVisionBuiltin(b, VisionConfig{Endpoint: "http://x", APIKey: "k", Model: ""}); err == nil {
		t.Error("expected error when Model missing")
	}
}
