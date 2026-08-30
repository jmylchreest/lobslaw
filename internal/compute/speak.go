package compute

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Speech is the first generation modality, and the easiest one: it is
// synchronous. A vendor hands back audio in the response rather than a
// job handle, so it needs none of the async machinery — but it DOES
// produce an artifact, so it is the first real user of the resolver.
//
// It is also the modality that shows why Artifact exists at all. Audio
// bytes are not text: they cannot go in a tool result the model reads,
// they have to land somewhere the channel layer can attach from.

// SpeakRequest is one text-to-speech call.
type SpeakRequest struct {
	Model string
	Text  string

	// Voice is vendor-specific and deliberately a bare string. Every
	// provider has its own set and there is no useful common
	// vocabulary to type here.
	Voice string

	// Format is the container the caller wants ("mp3", "wav", "opus").
	// Empty picks the driver's default.
	Format string

	// Speed is a multiplier where supported. Zero means the driver's
	// default rather than "stopped".
	Speed float32
}

// SpeakDriver renders text as audio.
//
// It returns an Artifact rather than bytes so the three delivery modes
// stay open: a vendor that hands back a URL, or writes into an
// operator bucket, fits without changing this signature. Today's
// implementations all return inline bytes, and the resolver makes that
// difference invisible.
type SpeakDriver interface {
	Speak(ctx context.Context, req SpeakRequest) (*Artifact, error)
}

// OpenAISpeakConfig wires the /v1/audio/speech shape.
type OpenAISpeakConfig struct {
	Endpoint   string
	Model      string
	Voice      string
	Format     string
	Credential Credential
	HTTPClient *http.Client
}

// DefaultSpeakFormat is mp3: universally playable, and small enough
// that a long reply does not become a large attachment.
const DefaultSpeakFormat = "mp3"

// OpenAISpeakDriver speaks the /v1/audio/speech protocol — OpenAI's,
// and cloned by enough vendors (self-hosted Kokoro and Piper wrappers
// among them) that it is the sensible first implementation.
type OpenAISpeakDriver struct {
	cfg    OpenAISpeakConfig
	client *http.Client
}

func NewOpenAISpeakDriver(cfg OpenAISpeakConfig) (*OpenAISpeakDriver, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("speak: endpoint required")
	}
	if cfg.Credential == nil {
		return nil, fmt.Errorf("speak: credential required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("speak: model required")
	}
	c := cfg.HTTPClient
	if c == nil {
		// Generous: synthesising a long reply is slower than a chat
		// turn, and a truncated audio file is worse than a slow one.
		c = &http.Client{Timeout: 3 * time.Minute}
	}
	return &OpenAISpeakDriver{cfg: cfg, client: c}, nil
}

type speakWire struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice,omitempty"`
	ResponseFormat string  `json:"response_format,omitempty"`
	Speed          float32 `json:"speed,omitempty"`
}

func (d *OpenAISpeakDriver) Speak(ctx context.Context, req SpeakRequest) (*Artifact, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		// Permanent by construction: an empty prompt fails identically
		// against every provider, so failing over would just spend the
		// backup's quota to learn the same thing.
		return nil, Permanent(fmt.Errorf("speak: nothing to say"))
	}

	format := cmp.Or(req.Format, d.cfg.Format, DefaultSpeakFormat)
	body, err := json.Marshal(speakWire{
		Model:          cmp.Or(req.Model, d.cfg.Model),
		Input:          text,
		Voice:          cmp.Or(req.Voice, d.cfg.Voice),
		ResponseFormat: format,
		Speed:          req.Speed,
	})
	if err != nil {
		return nil, Permanent(fmt.Errorf("speak: marshal: %w", err))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, Permanent(fmt.Errorf("speak: build request: %w", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if err := d.cfg.Credential.Apply(ctx, httpReq); err != nil {
		return nil, Permanent(fmt.Errorf("speak: apply credential: %w", err))
	}

	resp, err := d.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, Permanent(fmt.Errorf("speak: %w", ctx.Err()))
		}
		return nil, Transient(fmt.Errorf("speak: http: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, Transient(fmt.Errorf("speak: read: %w", readErr))
	}
	if resp.StatusCode >= 400 {
		return nil, &DriverError{
			Class: ClassifyHTTPStatus(resp.StatusCode, string(raw)),
			Err:   fmt.Errorf("speak: HTTP %d: %s", resp.StatusCode, TruncateBodyFor(raw, 512)),
		}
	}
	if len(raw) == 0 {
		// A 200 with no audio is a provider bug, not a caller bug, so
		// it is worth trying the backup.
		return nil, Transient(fmt.Errorf("speak: provider returned no audio"))
	}

	return &Artifact{
		Kind:  ArtifactInline,
		Bytes: raw,
		MIME:  mimeForAudioFormat(format),
	}, nil
}

func mimeForAudioFormat(f string) string {
	switch strings.ToLower(strings.TrimSpace(f)) {
	case "wav":
		return "audio/wav"
	case "opus", "ogg":
		return "audio/ogg"
	case "flac":
		return "audio/flac"
	case "aac":
		return "audio/aac"
	case "pcm":
		return "audio/pcm"
	default:
		return "audio/mpeg"
	}
}
