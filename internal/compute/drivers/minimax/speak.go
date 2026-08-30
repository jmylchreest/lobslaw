package minimax

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// DefaultSpeakEndpoint is the text-to-audio path.
const DefaultSpeakEndpoint = "https://api.minimax.io/v1/t2a_v2"

// defaultVoice is an English narrator. Named rather than left empty
// because MiniMax refuses a request with no voice_setting, and
// "pick something sensible" is a better default than a 400 on the
// first thing anybody asks the assistant to say.
const defaultVoice = "English_expressive_narrator"

// defaultSampleRate matches the vendor's own examples. Carried
// explicitly because audio_setting is not optional.
const defaultSampleRate = 32000

// defaultBitrate and defaultVolume follow the vendor's own examples.
// Volume runs 1-10, and 1 is a whisper rather than a neutral level —
// a "sensible default" that nobody can hear is not one.
const (
	defaultBitrate = 128000
	defaultVolume  = 5
)

// speed bounds accepted by the vendor. Outside them the request is
// refused, so a caller asking for 4x gets the fastest allowed rather
// than an error about a number they did not choose deliberately.
const (
	minSpeed = 0.5
	maxSpeed = 2.0
)

// SpeakConfig wires one driver instance.
type SpeakConfig struct {
	Endpoint   string
	Model      string
	Voice      string
	Format     string
	Credential compute.Credential
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// SpeakDriver renders text as audio through MiniMax.
type SpeakDriver struct {
	cfg    SpeakConfig
	client *http.Client
}

// NewSpeak builds a driver.
func NewSpeak(cfg SpeakConfig) (*SpeakDriver, error) {
	if cfg.Credential == nil {
		return nil, fmt.Errorf("minimax speak: credential required")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultSpeakEndpoint
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &SpeakDriver{cfg: cfg, client: compute.HTTPClientOr(cfg.HTTPClient)}, nil
}

type speakRequest struct {
	Model string `json:"model"`
	Text  string `json:"text"`
	// Stream stays false: the streaming form chunks base64 PCM and
	// this driver hands back one finished artifact.
	Stream bool `json:"stream"`
	// OutputFormat is pinned rather than left to the vendor default,
	// because it changes the SHAPE of the reply: "hex" puts a string
	// at data.audio and "url" puts an object there. Relying on the
	// default means a vendor changing it turns every request into an
	// unmarshal error, and inline bytes are what the resolver wants
	// anyway — a URL here expires in 24h and costs a second fetch.
	OutputFormat string `json:"output_format"`
	// LanguageBoost lets the vendor detect the language rather than
	// assuming the voice's. Cheap, and the difference is audible on
	// anything not English.
	LanguageBoost string       `json:"language_boost,omitempty"`
	VoiceSetting  voiceSetting `json:"voice_setting"`
	AudioSetting  audioSetting `json:"audio_setting"`
}

type voiceSetting struct {
	VoiceID string  `json:"voice_id"`
	Speed   float32 `json:"speed"`
	Vol     float32 `json:"vol"`
	Pitch   int     `json:"pitch"`
}

type audioSetting struct {
	SampleRate int    `json:"sample_rate"`
	Bitrate    int    `json:"bitrate"`
	Format     string `json:"format"`
}

type speakResponse struct {
	Data *struct {
		// Audio is HEX, not base64 — the one thing about this API
		// that will silently produce garbage if you assume otherwise,
		// because base64-decoding hex mostly succeeds and yields
		// bytes that are not audio.
		Audio string `json:"audio"`
	} `json:"data"`
	ExtraInfo struct {
		AudioSize int `json:"audio_size"`
	} `json:"extra_info"`
	BaseResp baseResp `json:"base_resp"`
}

// Speak renders text and returns the audio inline.
func (d *SpeakDriver) Speak(ctx context.Context, req compute.SpeakRequest) (*compute.Artifact, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return nil, compute.Permanent(fmt.Errorf("minimax speak: text required"))
	}
	format := cmp.Or(req.Format, d.cfg.Format, "mp3")
	body, err := json.Marshal(speakRequest{
		Model:         cmp.Or(req.Model, d.cfg.Model),
		Text:          text,
		Stream:        false,
		OutputFormat:  "hex",
		LanguageBoost: "auto",
		VoiceSetting: voiceSetting{
			VoiceID: cmp.Or(req.Voice, d.cfg.Voice, defaultVoice),
			Speed:   clampSpeed(req.Speed),
			Vol:     defaultVolume,
		},
		AudioSetting: audioSetting{
			SampleRate: defaultSampleRate,
			Bitrate:    defaultBitrate,
			Format:     format,
		},
	})
	if err != nil {
		return nil, compute.Permanent(fmt.Errorf("minimax speak: marshal: %w", err))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, compute.Permanent(fmt.Errorf("minimax speak: build request: %w", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if err := d.cfg.Credential.Apply(ctx, httpReq); err != nil {
		return nil, compute.Permanent(fmt.Errorf("minimax speak: apply credential: %w", err))
	}

	resp, err := d.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, compute.Permanent(fmt.Errorf("minimax speak: %w", ctx.Err()))
		}
		return nil, compute.Transient(fmt.Errorf("minimax speak: http: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, compute.Transient(fmt.Errorf("minimax speak: read: %w", readErr))
	}
	if resp.StatusCode >= 400 {
		return nil, &compute.DriverError{
			Class: compute.ClassifyHTTPStatus(resp.StatusCode, string(raw)),
			Err:   fmt.Errorf("minimax speak: HTTP %d: %s", resp.StatusCode, truncate(raw)),
		}
	}

	var out speakResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, compute.Permanent(fmt.Errorf("minimax speak: malformed response: %w", err))
	}
	// Before the payload, for the same reason as image generation: a
	// failure arrives as HTTP 200 with data:null.
	if out.BaseResp.StatusCode != 0 {
		return nil, classifyStatus(out.BaseResp)
	}
	if out.Data == nil || out.Data.Audio == "" {
		return nil, compute.Transient(fmt.Errorf("minimax speak: response carried no audio"))
	}
	decoded, err := hex.DecodeString(out.Data.Audio)
	if err != nil {
		return nil, compute.Permanent(fmt.Errorf("minimax speak: decode hex audio: %w", err))
	}
	return &compute.Artifact{
		Kind:  compute.ArtifactInline,
		Bytes: decoded,
		MIME:  mimeForFormat(format),
	}, nil
}

func mimeForFormat(format string) string {
	switch strings.ToLower(format) {
	case "wav":
		return "audio/wav"
	case "pcm":
		return "audio/pcm"
	case "flac":
		return "audio/flac"
	default:
		return "audio/mpeg"
	}
}

// clampSpeed keeps the multiplier inside what the vendor accepts.
//
// Zero means "the driver's default", NOT "stopped" — SpeakRequest
// says so, and sending 0 would either be refused or produce silence.
func clampSpeed(speed float32) float32 {
	switch {
	case speed <= 0:
		return 1
	case speed < minSpeed:
		return minSpeed
	case speed > maxSpeed:
		return maxSpeed
	default:
		return speed
	}
}
