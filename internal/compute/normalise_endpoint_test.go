package compute

import "testing"

// [[compute.providers]] endpoints are BASE urls — the form every
// vendor's documentation quotes. LLMClient has always appended the
// path; the modality builtins inherit the same field and used it
// verbatim, so read_image against a provider entry POSTed to the bare
// base and got "HTTP 404: " with an empty body.
//
// The agent then reported it as a missing FILE, because that is what
// a 404 looks like from where it was standing — sending the operator
// to check a path that was correct all along.

func TestABaseURLGetsThePath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ given, path, want string }{
		{"https://api.example.com/v1", ChatCompletionsPath, "https://api.example.com/v1/chat/completions"},
		{"https://api.example.com/v1/", ChatCompletionsPath, "https://api.example.com/v1/chat/completions"},
		// Whisper posts somewhere else entirely, which is why the
		// suffix is a parameter and not a constant inside the helper.
		{"https://api.example.com/v1", TranscriptionsPath, "https://api.example.com/v1/audio/transcriptions"},
	} {
		if got := NormaliseEndpoint(tc.given, tc.path); got != tc.want {
			t.Errorf("NormaliseEndpoint(%q, %q) = %q, want %q", tc.given, tc.path, got, tc.want)
		}
	}
}

// Idempotent, so an operator who wrote the full URL is not
// second-guessed — and so a second pass over an already-normalised
// value cannot produce /chat/completions/chat/completions.
func TestACompleteURLIsLeftAlone(t *testing.T) {
	t.Parallel()
	full := "https://api.example.com/v1/chat/completions"
	if got := NormaliseEndpoint(full, ChatCompletionsPath); got != full {
		t.Errorf("a complete URL became %q", got)
	}
	if got := NormaliseEndpoint(NormaliseEndpoint("https://x/v1", ChatCompletionsPath), ChatCompletionsPath); got != "https://x/v1/chat/completions" {
		t.Errorf("not idempotent: %q", got)
	}
}

// The drivers that inherit a provider endpoint must all normalise it.
// Vision was the one that shipped broken; audio and pdf read the same
// field the same way and would have failed identically the first time
// somebody sent a voice note.
func TestEveryDriverInheritingAProviderEndpointNormalisesIt(t *testing.T) {
	t.Parallel()
	const base = "https://api.example.com/v1"
	cred := NewHeaderCredential("x-api-key", "k")

	vd, err := OpenAIVisionFactory(VisionDriverConfig{Endpoint: base, Model: "m", Credential: cred})
	if err != nil {
		t.Fatal(err)
	}
	if got := vd.(*openAIVisionDriver).cfg.Endpoint; got != base+ChatCompletionsPath {
		t.Errorf("vision endpoint = %q", got)
	}

	ad, err := ChatMultimodalAudioFactory(AudioDriverConfig{Endpoint: base, Model: "m", Credential: cred})
	if err != nil {
		t.Fatal(err)
	}
	if got := ad.(*chatMultimodalAudioDriver).cfg.Endpoint; got != base+ChatCompletionsPath {
		t.Errorf("chat-multimodal audio endpoint = %q", got)
	}

	wd, err := WhisperAudioFactory(AudioDriverConfig{Endpoint: base, Model: "m", Credential: cred})
	if err != nil {
		t.Fatal(err)
	}
	if got := wd.(*whisperAudioDriver).cfg.Endpoint; got != base+TranscriptionsPath {
		t.Errorf("whisper endpoint = %q", got)
	}
}
