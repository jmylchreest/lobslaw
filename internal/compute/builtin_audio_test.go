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

func TestReadAudioWhisperFormat(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	audioPath := filepath.Join(tmp, "voice.ogg")
	_ = os.WriteFile(audioPath, []byte("OggS\x00\x02"), 0o600)

	var gotContentType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"text":"hey john"}`))
	}))
	t.Cleanup(srv.Close)

	b := NewBuiltins()
	if err := RegisterAudioBuiltin(b, AudioConfig{
		Endpoint:    srv.URL,
		Model:       "whisper-1",
		APIKey:      "k",
		Format:      AudioFormatWhisper,
		AllowedRoot: tmp,
	}); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("read_audio")
	out, code, err := fn(context.Background(), map[string]string{"path": audioPath, "language": "en"})
	if err != nil || code != 0 {
		t.Fatalf("dispatch: code=%d err=%v", code, err)
	}
	var resp map[string]any
	_ = json.Unmarshal(out, &resp)
	if resp["content"] != "hey john" {
		t.Errorf("transcript = %q, want hey john", resp["content"])
	}
	if !strings.HasPrefix(gotContentType, "multipart/form-data") {
		t.Errorf("expected multipart Content-Type; got %q", gotContentType)
	}
	if !strings.Contains(string(gotBody), `name="file"`) || !strings.Contains(string(gotBody), `name="model"`) {
		t.Errorf("expected multipart fields; body = %s", gotBody)
	}
	if !strings.Contains(string(gotBody), `audio/ogg`) {
		t.Errorf("expected audio/ogg part header for .ogg; body = %s", gotBody)
	}
}

func TestReadAudioOpenRouterChatFormat(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	audioPath := filepath.Join(tmp, "voice.ogg")
	_ = os.WriteFile(audioPath, []byte("OggS\x00\x02"), 0o600)

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hello there"}}]}`))
	}))
	t.Cleanup(srv.Close)

	b := NewBuiltins()
	if err := RegisterAudioBuiltin(b, AudioConfig{
		Endpoint:    srv.URL,
		Model:       "google/gemini-2.0-flash-001",
		APIKey:      "k",
		Format:      AudioFormatChatMultimodal,
		AllowedRoot: tmp,
	}); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("read_audio")
	out, code, err := fn(context.Background(), map[string]string{"path": audioPath})
	if err != nil || code != 0 {
		t.Fatalf("dispatch: code=%d err=%v", code, err)
	}
	var resp map[string]any
	_ = json.Unmarshal(out, &resp)
	if resp["content"] != "hello there" {
		t.Errorf("transcript = %q", resp["content"])
	}
	if !strings.Contains(string(gotBody), `"input_audio"`) || !strings.Contains(string(gotBody), `"format":"ogg"`) {
		t.Errorf("expected input_audio with format=ogg; got %s", gotBody)
	}
}

func TestReadAudioRefusesPathOutsideRoot(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("vision endpoint should not be hit on path-scope failure")
	}))
	t.Cleanup(srv.Close)

	b := NewBuiltins()
	if err := RegisterAudioBuiltin(b, AudioConfig{
		Endpoint:    srv.URL,
		Model:       "whisper-1",
		APIKey:      "k",
		AllowedRoot: "/workspace/incoming",
	}); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	out := filepath.Join(tmp, "evil.ogg")
	_ = os.WriteFile(out, []byte("x"), 0o600)
	fn, _ := b.Get("read_audio")
	_, code, err := fn(context.Background(), map[string]string{"path": out})
	if err == nil {
		t.Error("expected error")
	}
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
}
