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

// The two shapes are built as drivers now. read_audio used to switch
// on an AudioFormat once to validate and once to dispatch.

func whisperAudioFor(t *testing.T, endpoint string) compute.AudioDriver {
	t.Helper()
	d, err := compute.WhisperAudioFactory(compute.AudioDriverConfig{
		Endpoint:   endpoint,
		Model:      "whisper-1",
		Credential: compute.NewBearerCredential("fake"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func chatMultimodalAudioFor(t *testing.T, endpoint string) compute.AudioDriver {
	t.Helper()
	d, err := compute.ChatMultimodalAudioFactory(compute.AudioDriverConfig{
		Endpoint:   endpoint,
		Model:      "google/gemini-2.0-flash-001",
		Credential: compute.NewBearerCredential("fake"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

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
		Driver:      whisperAudioFor(t, srv.URL),
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
		Driver:      chatMultimodalAudioFor(t, srv.URL),
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
		Driver:      whisperAudioFor(t, srv.URL),
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

// A config with no driver cannot transcribe anything, and the failure
// belongs at boot rather than the first time somebody sends a voice
// note.
func TestReadAudioRequiresADriver(t *testing.T) {
	t.Parallel()
	b := NewBuiltins()
	err := RegisterAudioBuiltin(b, AudioConfig{
		Endpoint: "http://x", APIKey: "k", Model: "whisper-1",
	})
	if err == nil {
		t.Fatal("a config with no driver was accepted")
	}
	if !strings.Contains(err.Error(), "Driver") {
		t.Errorf("err = %q; it does not say what is missing", err)
	}
}

// Both factories reject a config they cannot serve, so the error names
// the endpoint rather than arriving as a nil dereference later.
func TestAudioFactoriesNeedAnEndpointAndModel(t *testing.T) {
	t.Parallel()
	for name, f := range map[string]compute.AudioDriverFactory{
		"whisper":        compute.WhisperAudioFactory,
		"chatmultimodal": compute.ChatMultimodalAudioFactory,
	} {
		if _, err := f(compute.AudioDriverConfig{Model: "m"}); err == nil {
			t.Errorf("%s: no endpoint was accepted", name)
		}
		if _, err := f(compute.AudioDriverConfig{Endpoint: "http://x"}); err == nil {
			t.Errorf("%s: no model was accepted", name)
		}
	}
}

// The mock serves without touching the network, which is what lets a
// node whose every provider is driver = "mock" run a full turn with no
// egress.
func TestTheMockAudioDriverNeedsNoNetwork(t *testing.T) {
	t.Parallel()
	d, err := compute.MockAudioFactory(compute.AudioDriverConfig{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Transcribe(t.Context(), compute.AudioRequest{Filename: "voice.ogg"})
	if err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Error("the mock produced no transcript")
	}
}

// Both shapes must present the credential. The bearer header used to
// be set inline in each transcribe function; moving it behind
// Credential is exactly the kind of change that silently drops it, and
// an endpoint that answers 401 looks like a provider outage.
func TestBothAudioDriversPresentTheirCredential(t *testing.T) {
	t.Parallel()
	for name, build := range map[string]func(string) (compute.AudioDriver, error){
		"whisper": func(url string) (compute.AudioDriver, error) {
			return compute.WhisperAudioFactory(compute.AudioDriverConfig{
				Endpoint: url, Model: "whisper-1",
				Credential: compute.NewBearerCredential("sk-test"),
			})
		},
		"chatmultimodal": func(url string) (compute.AudioDriver, error) {
			return compute.ChatMultimodalAudioFactory(compute.AudioDriverConfig{
				Endpoint: url, Model: "m",
				Credential: compute.NewBearerCredential("sk-test"),
			})
		},
	} {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(
				`{"text":"hello","choices":[{"message":{"content":"hello"}}]}`))
		}))

		d, err := build(srv.URL)
		if err != nil {
			srv.Close()
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := d.Transcribe(t.Context(), compute.AudioRequest{
			Filename: "voice.ogg", Data: []byte("audio"),
		}); err != nil {
			srv.Close()
			t.Fatalf("%s: %v", name, err)
		}
		srv.Close()

		if gotAuth != "Bearer sk-test" {
			t.Errorf("%s: Authorization = %q; the credential was dropped", name, gotAuth)
		}
	}
}
