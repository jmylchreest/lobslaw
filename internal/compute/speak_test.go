package compute

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func speakServer(t *testing.T, status int, body []byte, record *speakWire) *httptest.Server {
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

func newSpeakDriver(t *testing.T, endpoint string) *OpenAISpeakDriver {
	t.Helper()
	d, err := NewOpenAISpeakDriver(OpenAISpeakConfig{
		Endpoint:   endpoint,
		Model:      "tts-1",
		Voice:      "alloy",
		Credential: NewBearerCredential("k"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Audio comes back as an Artifact, not bytes, so a vendor that later
// returns a URL or writes into a bucket fits without changing the
// interface. Today it is inline, and the MIME has to match the format
// actually requested or the file lands with the wrong extension.
func TestSpeakReturnsAnInlineArtifact(t *testing.T) {
	t.Parallel()
	var sent speakWire
	srv := speakServer(t, http.StatusOK, []byte("ID3-fake-mp3"), &sent)

	art, err := newSpeakDriver(t, srv.URL).Speak(context.Background(), SpeakRequest{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if art.Kind != ArtifactInline || string(art.Bytes) != "ID3-fake-mp3" {
		t.Errorf("artifact = %+v, want inline audio bytes", art)
	}
	if art.MIME != "audio/mpeg" {
		t.Errorf("MIME = %q, want audio/mpeg for the default mp3 format", art.MIME)
	}
	if sent.Input != "hello" || sent.Model != "tts-1" || sent.Voice != "alloy" {
		t.Errorf("request did not carry the configured defaults: %+v", sent)
	}
	if sent.ResponseFormat != DefaultSpeakFormat {
		t.Errorf("response_format = %q, want %q", sent.ResponseFormat, DefaultSpeakFormat)
	}
}

func TestSpeakFormatDrivesMIME(t *testing.T) {
	t.Parallel()
	for format, want := range map[string]string{
		"wav": "audio/wav", "opus": "audio/ogg", "flac": "audio/flac", "mp3": "audio/mpeg",
	} {
		srv := speakServer(t, http.StatusOK, []byte("audio"), nil)
		art, err := newSpeakDriver(t, srv.URL).Speak(context.Background(),
			SpeakRequest{Text: "x", Format: format})
		if err != nil {
			t.Fatal(err)
		}
		if art.MIME != want {
			t.Errorf("format %q → MIME %q, want %q", format, art.MIME, want)
		}
	}
}

// The failover chain reads the class, so speak has to classify like
// every other driver on the waist.
func TestSpeakClassifiesFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   FailureClass
	}{
		{"server error", 503, `{"error":"upstream"}`, FailureTransient},
		{"quota", 402, `{"error":"credit balance is too low"}`, FailureQuotaExhausted},
		{"bad voice", 400, `{"error":"unknown voice"}`, FailurePermanent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := speakServer(t, tc.status, []byte(tc.body), nil)
			_, err := newSpeakDriver(t, srv.URL).Speak(context.Background(), SpeakRequest{Text: "x"})
			if err == nil {
				t.Fatalf("HTTP %d produced no error", tc.status)
			}
			if got := ClassifyFailure(err); got != tc.want {
				t.Errorf("HTTP %d classified %s, want %s", tc.status, got, tc.want)
			}
		})
	}
}

// A 200 carrying no audio is a provider bug rather than a caller one,
// so it is worth trying the backup rather than surfacing as success.
func TestSpeakTreatsEmptyAudioAsTransient(t *testing.T) {
	t.Parallel()
	srv := speakServer(t, http.StatusOK, nil, nil)
	_, err := newSpeakDriver(t, srv.URL).Speak(context.Background(), SpeakRequest{Text: "x"})
	if err == nil {
		t.Fatal("an empty response was accepted as audio")
	}
	if got := ClassifyFailure(err); got != FailureTransient {
		t.Errorf("classified %s, want transient", got)
	}
}

// Empty text fails identically everywhere, so failing over would just
// spend the backup's quota to learn the same thing.
func TestSpeakRejectsEmptyTextPermanently(t *testing.T) {
	t.Parallel()
	srv := speakServer(t, http.StatusOK, []byte("audio"), nil)
	_, err := newSpeakDriver(t, srv.URL).Speak(context.Background(), SpeakRequest{Text: "   "})
	if err == nil {
		t.Fatal("empty text was sent to the provider")
	}
	if got := ClassifyFailure(err); got != FailurePermanent {
		t.Errorf("classified %s, want permanent", got)
	}
}
