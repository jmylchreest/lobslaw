// Package elevenlabs implements the speak modality against
// ElevenLabs.
//
// It is the second speak driver, and its value is that it disagrees
// with the first everywhere that matters: the voice is a path segment
// rather than a body field, auth is xi-api-key rather than a bearer
// token, the output format is a query parameter naming a codec and a
// sample rate together, and there is no "model" in the OpenAI sense —
// model_id selects a synthesis engine while the voice selects the
// speaker.
//
// One driver always fits its own interface. If SpeakDriver carries
// this one unchanged, it is an interface rather than a description of
// OpenAI.
package elevenlabs

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

const (
	// DriverName is the string config selects this with.
	DriverName = "elevenlabs"

	// DefaultBaseURL has the voice id appended.
	DefaultBaseURL = "https://api.elevenlabs.io/v1/text-to-speech/"

	// DefaultModel is the multilingual engine. Distinct from the
	// voice: this picks HOW, the voice picks WHO.
	DefaultModel = "eleven_multilingual_v2"

	// DefaultVoice is ElevenLabs' stock "Rachel" voice, used when the
	// operator names none. A voice is required by the API, so
	// defaulting beats a 404 on the first call.
	DefaultVoice = "21m00Tcm4TlvDq8ikWAM"
)

type Config struct {
	// BaseURL has the voice id appended to it.
	BaseURL    string
	Model      string
	Voice      string
	Format     string
	Credential compute.Credential
	HTTPClient *http.Client
}

type Driver struct {
	cfg    Config
	client *http.Client
}

func New(cfg Config) (*Driver, error) {
	if cfg.Credential == nil {
		return nil, fmt.Errorf("elevenlabs: credential required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if !strings.HasSuffix(cfg.BaseURL, "/") {
		cfg.BaseURL += "/"
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.Voice == "" {
		cfg.Voice = DefaultVoice
	}
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 3 * time.Minute}
	}
	return &Driver{cfg: cfg, client: c}, nil
}

type wireRequest struct {
	Text    string `json:"text"`
	ModelID string `json:"model_id,omitempty"`
}

func (d *Driver) Speak(ctx context.Context, req compute.SpeakRequest) (*compute.Artifact, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return nil, compute.Permanent(fmt.Errorf("elevenlabs: nothing to say"))
	}

	voice := strings.TrimSpace(req.Voice)
	if voice == "" {
		voice = d.cfg.Voice
	}
	model := req.Model
	if model == "" {
		model = d.cfg.Model
	}

	body, err := json.Marshal(wireRequest{Text: text, ModelID: model})
	if err != nil {
		return nil, compute.Permanent(fmt.Errorf("elevenlabs: marshal: %w", err))
	}

	// The voice is part of the PATH. url.PathEscape rather than string
	// concatenation because the voice reaches us from a tool argument
	// the model chose, and a value containing a slash would otherwise
	// address a different endpoint entirely.
	endpoint := d.cfg.BaseURL + url.PathEscape(voice)
	outputFormat := elevenFormat(cmp.Or(req.Format, d.cfg.Format))
	if outputFormat != "" {
		endpoint += "?output_format=" + url.QueryEscape(outputFormat)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, compute.Permanent(fmt.Errorf("elevenlabs: build request: %w", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "audio/mpeg")
	if err := d.cfg.Credential.Apply(ctx, httpReq); err != nil {
		return nil, compute.Permanent(fmt.Errorf("elevenlabs: apply credential: %w", err))
	}

	resp, err := d.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, compute.Permanent(fmt.Errorf("elevenlabs: %w", ctx.Err()))
		}
		return nil, compute.Transient(fmt.Errorf("elevenlabs: http: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, compute.Transient(fmt.Errorf("elevenlabs: read: %w", readErr))
	}
	if resp.StatusCode >= 400 {
		return nil, &compute.DriverError{
			Class: compute.ClassifyHTTPStatus(resp.StatusCode, string(raw)),
			Err:   fmt.Errorf("elevenlabs: HTTP %d: %s", resp.StatusCode, truncate(raw)),
		}
	}
	if len(raw) == 0 {
		return nil, compute.Transient(fmt.Errorf("elevenlabs: returned no audio"))
	}

	return &compute.Artifact{
		Kind:  compute.ArtifactInline,
		Bytes: raw,
		MIME:  mimeForOutputFormat(outputFormat),
	}, nil
}

// elevenFormat translates the container names the tool accepts into
// ElevenLabs' codec_rate_bitrate spelling. An operator who already
// knows the vendor's vocabulary can pass it through untouched.
func elevenFormat(f string) string {
	switch strings.ToLower(strings.TrimSpace(f)) {
	case "", "mp3":
		return "mp3_44100_128"
	case "opus", "ogg":
		return "opus_48000_64"
	case "pcm", "wav":
		return "pcm_44100"
	case "ulaw":
		return "ulaw_8000"
	default:
		return f
	}
}

func mimeForOutputFormat(f string) string {
	switch {
	case strings.HasPrefix(f, "opus"):
		return "audio/ogg"
	case strings.HasPrefix(f, "pcm"):
		return "audio/wav"
	case strings.HasPrefix(f, "ulaw"):
		return "audio/basic"
	default:
		return "audio/mpeg"
	}
}

func truncate(b []byte) string {
	const max = 512
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…[truncated]"
}
