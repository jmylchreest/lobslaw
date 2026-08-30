package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// AudioConfig wires the read_audio (STT) builtin. Format selects
// between Whisper-style multipart (default) and OpenRouter's
// chat-completions-multimodal shape. For self-hosted Parakeet /
// faster-whisper sidecars: leave Format empty (whisper) and point
// Endpoint at the local URL.
type AudioConfig struct {
	Endpoint string
	Model    string
	APIKey   string
	// Driver is the resolved wire protocol. This was an AudioFormat
	// switched on twice — once to validate, once to dispatch.
	Driver      compute.AudioDriver
	AllowedRoot string
	HTTPClient  *http.Client

	// Label is the provider's config label, used as the health key so
	// a demotion is shared with every other modality that reaches the
	// same endpoint. Empty opts out of health tracking.
	Label string

	// TrustTier is the provider's declared tier, checked against the
	// soul's min_trust_tier before this provider is used. Empty fails
	// any set floor: an undeclared tier is not evidence of a high one.
	TrustTier types.TrustTier
}

// RegisterAudioBuiltin installs read_audio. Required-fields check
// matches the vision builtin so misconfigurations surface loudly.
// Variadic for the same reason as vision: one config is a single
// provider, several are a failover chain in the order given.
func RegisterAudioBuiltin(b *Builtins, cfgs ...AudioConfig) error {
	if len(cfgs) == 0 {
		return errors.New("read_audio: at least one provider config required")
	}
	handlers := make([]compute.FailoverHandler, 0, len(cfgs))
	for _, cfg := range cfgs {
		if cfg.Endpoint == "" || cfg.APIKey == "" {
			return errors.New("read_audio: Endpoint and APIKey both required")
		}
		if cfg.Model == "" {
			return errors.New("read_audio: Model required (e.g. \"whisper-1\", \"speech-01\", \"google/gemini-2.0-flash-001\")")
		}

		if cfg.Driver == nil {
			return errors.New("read_audio: Driver required (resolve it from the DriverSet)")
		}
		if cfg.AllowedRoot == "" {
			cfg.AllowedRoot = DefaultIncomingDir
		}
		client := cfg.HTTPClient
		if client == nil {
			client = &http.Client{Timeout: 120 * time.Second}
		}
		handlers = append(handlers, compute.FailoverHandler{Label: cfg.Label, Tier: cfg.TrustTier, Fn: newReadAudioHandler(cfg, client)})
	}
	return b.Register("read_audio", compute.FailoverBuiltin("read_audio", nil, b.Health(), b.TrustFloor(), handlers...))
}

// AudioToolDef is the ToolDef registered alongside the builtin.
func AudioToolDef() *types.ToolDef {
	return &types.ToolDef{
		Name:        "read_audio",
		Path:        compute.BuiltinScheme + "read_audio",
		Description: "Transcribe an audio file (voice note, recording) at a local path to text. Channel layer downloads inbound voice/audio attachments to /workspace/incoming/<turn>/<file> and surfaces the path via [user attached: voice ... path=...]. When the user sends a voice note or audio file, ALWAYS call read_audio with that path before answering — the main model can't ingest audio directly. Optional language hint (BCP-47, e.g. \"en\", \"de\") improves accuracy on accented or non-English speech. Returns the transcript as content.",
		ParametersSchema: []byte(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Absolute path to the audio file (typically /workspace/incoming/<turn>/<file>)."},
				"language": {"type": "string", "description": "Optional BCP-47 language hint (e.g. \"en\", \"de\"). Empty → autodetect."}
			},
			"required": ["path"],
			"additionalProperties": false
		}`),
		RiskTier: types.RiskCommunicating,
	}
}

func newReadAudioHandler(cfg AudioConfig, client *http.Client) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		path := strings.TrimSpace(args["path"])
		if path == "" {
			return nil, 2, errors.New("read_audio: path is required")
		}
		language := strings.TrimSpace(args["language"])

		// The shared chain: mounts, absolute, cluster-internal,
		// hardline, then the operator's policy.d. This used to be an
		// AllowedRoot prefix test and nothing else — which meant a
		// modality tool could read a TLS key that read_file refuses,
		// as long as somebody had pointed a root at it.
		abs, payload, exit := guardReadWithin("read_audio", path, cfg.AllowedRoot)
		if exit != 0 {
			return payload, exit, nil
		}
		// AllowedRoot survives as a FURTHER narrowing. It is how the
		// channel layer pins these tools to the directory it drops
		// inbound attachments in, which is tighter than any mount.
		if cfg.AllowedRoot != "" {
			rootAbs, err := filepath.Abs(cfg.AllowedRoot)
			if err != nil {
				return nil, 1, fmt.Errorf("read_audio: resolve allowed root: %w", err)
			}
			if !pathWithinRoot(abs, rootAbs) {
				return nil, 2, fmt.Errorf("read_audio: path %q outside allowed root %q", abs, rootAbs)
			}
		}

		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, 2, fmt.Errorf("read_audio: read file: %w", err)
		}

		transcript, err := cfg.Driver.Transcribe(ctx, compute.AudioRequest{
			Filename: abs,
			Data:     data,
			Language: language,
		})
		if err != nil {
			return nil, 1, err
		}
		if transcript == "" {
			return nil, 1, errors.New("read_audio: provider returned empty transcript")
		}

		out, _ := json.Marshal(map[string]any{
			"path":    abs,
			"model":   cfg.Model,
			"content": transcript,
		})
		return out, 0, nil
	}
}
