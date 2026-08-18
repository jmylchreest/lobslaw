package compute

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// Speech-to-text, as a driver rather than a format enum.
//
// The same conversion vision had: `read_audio` switched on an
// AudioFormat once to validate and once to dispatch. Both shapes are
// OpenAI-descended — Whisper's multipart upload and OpenRouter's
// audio-on-chat — so neither earns its own package; they are factories
// here, like the OpenAI-shaped vision and image drivers.

// AudioRequest is one transcription.
type AudioRequest struct {
	// Filename is the base name, needed for the multipart field: some
	// servers infer the codec from the extension and reject an upload
	// called "file".
	Filename string
	// Data is the raw audio.
	Data []byte
	// Language is an optional BCP-47 hint. Empty lets the provider
	// detect it, which is usually better than a wrong guess.
	Language string
}

// AudioDriver transcribes one recording.
//
// Errors should be classified (DriverError / Transient) so the
// failover chain can tell "try the next provider" from "this fails
// everywhere".
type AudioDriver interface {
	Transcribe(ctx context.Context, req AudioRequest) (string, error)
}

// AudioDriverConfig is what every audio driver is built from.
type AudioDriverConfig struct {
	Endpoint   string
	Model      string
	Credential Credential
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// AudioDriverFactory builds one configured audio driver.
type AudioDriverFactory func(AudioDriverConfig) (AudioDriver, error)

// RegisterAudio adds a driver under name.
func (s *DriverSet) RegisterAudio(name string, f AudioDriverFactory) {
	if s.audio == nil {
		s.audio = make(map[string]AudioDriverFactory)
	}
	s.audio[normaliseDriverName(name)] = f
}

// Audio builds the named driver.
func (s *DriverSet) Audio(name string, cfg AudioDriverConfig) (AudioDriver, error) {
	key := normaliseDriverName(name)
	if key == "" {
		key = DriverOpenAI
	}
	f, ok := s.audio[key]
	if !ok {
		return nil, fmt.Errorf("unknown audio driver %q; available: %s",
			name, strings.Join(s.AudioNames(), ", "))
	}
	return f(cfg)
}

// AudioNames lists the registered audio drivers, sorted.
func (s *DriverSet) AudioNames() []string { return sortedKeys(s.audio) }

// WhisperAudioFactory adapts the /v1/audio/transcriptions multipart
// upload — OpenAI, MiniMax STT, and any self-hosted server exposing
// the same surface.
func WhisperAudioFactory(cfg AudioDriverConfig) (AudioDriver, error) {
	if err := checkAudioConfig(cfg); err != nil {
		return nil, err
	}
	// A base URL from the provider entry, plus the path this driver
	// actually posts to — a DIFFERENT one from the chat drivers, which
	// is exactly why the suffix is a parameter rather than a constant
	// buried in the helper.
	cfg.Endpoint = NormaliseEndpoint(cfg.Endpoint, TranscriptionsPath)
	return &whisperAudioDriver{cfg: cfg, client: HTTPClientOr(cfg.HTTPClient)}, nil
}

// ChatMultimodalAudioFactory adapts audio-on-chat: /v1/chat/completions
// with an input_audio content part, as OpenRouter exposes it. The
// assistant's reply is treated as the transcript.
func ChatMultimodalAudioFactory(cfg AudioDriverConfig) (AudioDriver, error) {
	if err := checkAudioConfig(cfg); err != nil {
		return nil, err
	}
	cfg.Endpoint = NormaliseEndpoint(cfg.Endpoint, ChatCompletionsPath)
	return &chatMultimodalAudioDriver{cfg: cfg, client: HTTPClientOr(cfg.HTTPClient)}, nil
}

// MockAudioFactory transcribes without touching the network.
func MockAudioFactory(_ AudioDriverConfig) (AudioDriver, error) {
	return mockAudioDriver{}, nil
}

type mockAudioDriver struct{}

func (mockAudioDriver) Transcribe(_ context.Context, req AudioRequest) (string, error) {
	return "a mock transcript of " + req.Filename, nil
}

func checkAudioConfig(cfg AudioDriverConfig) error {

	if cfg.Endpoint == "" {
		return errors.New("audio: endpoint required")
	}
	if cfg.Model == "" {
		return errors.New("audio: model required")
	}
	return nil
}
