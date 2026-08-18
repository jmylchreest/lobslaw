package minimax

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// THE ONE THAT SILENTLY CORRUPTS. MiniMax returns the audio as HEX,
// not base64. Assuming base64 mostly SUCCEEDS — it decodes to bytes
// — and writes a file that is not audio, with no error anywhere.
func TestTheAudioIsDecodedAsHexNotBase64(t *testing.T) {
	t.Parallel()
	// A real MP3 starts with an ID3 tag; a wrong decode loses it.
	want := []byte{0x49, 0x44, 0x33, 0x04, 0x00, 0x00, 0x11, 0x22}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":      map[string]any{"audio": hex.EncodeToString(want)},
			"base_resp": map[string]any{"status_code": 0, "status_msg": "success"},
		})
	}))
	defer srv.Close()

	d, err := NewSpeak(SpeakConfig{Endpoint: srv.URL, Model: "m", Credential: cred()})
	if err != nil {
		t.Fatal(err)
	}
	art, err := d.Speak(context.Background(), compute.SpeakRequest{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if string(art.Bytes) != string(want) {
		t.Errorf("audio = % x, want % x", art.Bytes, want)
	}
	if art.Kind != compute.ArtifactInline {
		t.Errorf("artifact kind = %v", art.Kind)
	}
}

// A failure arrives as HTTP 200 with data:null, exactly as it does
// for image generation.
func TestASpeakFailureOnA200IsReadFromTheBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":null,"base_resp":{"status_code":2013,"status_msg":"invalid params, unsupported voice"}}`))
	}))
	defer srv.Close()

	d, _ := NewSpeak(SpeakConfig{Endpoint: srv.URL, Model: "m", Credential: cred()})
	_, err := d.Speak(context.Background(), compute.SpeakRequest{Text: "hello"})
	if err == nil {
		t.Fatal("a 200-with-error-body was treated as success")
	}
	if !strings.Contains(err.Error(), "unsupported voice") {
		t.Errorf("the vendor's reason was dropped: %v", err)
	}
}

// voice_setting and audio_setting are not optional — MiniMax refuses
// a request without them, so a caller who names no voice must still
// get a working request rather than a 400 on the first thing the
// assistant is asked to say.
func TestADefaultVoiceAndFormatAreAlwaysSent(t *testing.T) {
	t.Parallel()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"data":{"audio":"4944"},"base_resp":{"status_code":0}}`))
	}))
	defer srv.Close()

	d, _ := NewSpeak(SpeakConfig{Endpoint: srv.URL, Model: "m", Credential: cred()})
	if _, err := d.Speak(context.Background(), compute.SpeakRequest{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	vs, _ := got["voice_setting"].(map[string]any)
	if vs["voice_id"] == "" || vs["voice_id"] == nil {
		t.Errorf("no voice_id sent: %v", got)
	}
	// Zero speed means "the driver's default", not "stopped".
	if speed, _ := vs["speed"].(float64); speed <= 0 {
		t.Errorf("speed sent as %v; zero would stop playback", vs["speed"])
	}
	as, _ := got["audio_setting"].(map[string]any)
	if as["format"] != "mp3" {
		t.Errorf("format = %v, want mp3", as["format"])
	}
}

func TestTheFormatPicksTheMIME(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ format, mime string }{
		{"mp3", "audio/mpeg"}, {"wav", "audio/wav"}, {"", "audio/mpeg"}, {"flac", "audio/flac"},
	} {
		if got := mimeForFormat(tc.format); got != tc.mime {
			t.Errorf("mimeForFormat(%q) = %q, want %q", tc.format, got, tc.mime)
		}
	}
}

// output_format changes the SHAPE of the reply: "hex" puts a string
// at data.audio, "url" puts an object there. Leaving it to the vendor
// default means a change on their side turns every request into an
// unmarshal error, so it is pinned.
func TestTheOutputFormatIsPinnedNotInherited(t *testing.T) {
	t.Parallel()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"data":{"audio":"4944"},"base_resp":{"status_code":0}}`))
	}))
	defer srv.Close()

	d, _ := NewSpeak(SpeakConfig{Endpoint: srv.URL, Model: "m", Credential: cred()})
	if _, err := d.Speak(context.Background(), compute.SpeakRequest{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if got["output_format"] != "hex" {
		t.Errorf("output_format = %v, want \"hex\" — the decoder expects a string at data.audio", got["output_format"])
	}
	if got["stream"] != false {
		t.Errorf("stream = %v; the streaming form chunks base64 PCM, which this decoder is not", got["stream"])
	}
}

// The vendor refuses a speed outside 0.5-2.0. A caller asking for 4x
// should get the fastest allowed rather than an error about a number
// they did not choose deliberately — and zero means "default", not
// "stopped".
func TestTheSpeedIsClampedToWhatTheVendorAccepts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		given, want float32
	}{
		{0, 1}, {-3, 1}, {0.1, 0.5}, {4, 2}, {1.5, 1.5}, {0.5, 0.5}, {2, 2},
	} {
		if got := clampSpeed(tc.given); got != tc.want {
			t.Errorf("clampSpeed(%v) = %v, want %v", tc.given, got, tc.want)
		}
	}
}
