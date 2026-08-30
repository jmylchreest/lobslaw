package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// SpeakConfig wires the speak builtin.
type SpeakConfig struct {
	Driver   compute.SpeakDriver
	Resolver *compute.ArtifactResolver

	// MaxChars bounds one synthesis. TTS is billed per character and a
	// model that decides to narrate a whole file would be expensive
	// and useless in equal measure. Zero picks DefaultSpeakMaxChars.
	MaxChars int

	// Label is the provider's config label, used as the health key so
	// a demotion is shared with every other modality that reaches the
	// same endpoint. Empty opts out of health tracking.
	Label string

	// TrustTier is the provider's declared tier, checked against the
	// soul's min_trust_tier before this provider is used. Empty fails
	// any set floor: an undeclared tier is not evidence of a high one.
	TrustTier types.TrustTier

	// Pricing is what this provider charges. Without it synthesised
	// speech costs the turn nothing and the spend cap cannot fire on
	// it — which matters more here than elsewhere, because TTS is the
	// one modality billed by the character and a model that decides to
	// narrate a file runs the meter fast.
	Pricing types.ProviderPricing

	// Model is carried for the cost record, so an audit says which
	// model was billed rather than only which provider.
	Model string

	// BilledTo distinguishes a prepaid plan from a balance. A plan has
	// no marginal cost per call, so pricing it as though it did would
	// inflate every turn this provider served — the meaningful number
	// there is the quantity drawn against the quota.
	BilledTo compute.Billing
}

// DefaultSpeakMaxChars is roughly a few minutes of speech — long
// enough for any reply a person would listen to, short enough that a
// runaway prompt cannot generate an hour of audio.
const DefaultSpeakMaxChars = 4000

// RegisterSpeakBuiltin installs the speak tool.
//
// Variadic drivers, same as the other modalities: several are a
// failover chain in priority order.
func RegisterSpeakBuiltin(b *Builtins, cfgs ...SpeakConfig) error {
	if len(cfgs) == 0 {
		return errors.New("speak: at least one provider config required")
	}
	handlers := make([]compute.FailoverHandler, 0, len(cfgs))
	for _, cfg := range cfgs {
		if cfg.Driver == nil {
			return errors.New("speak: Driver required")
		}
		if cfg.Resolver == nil {
			return errors.New("speak: Resolver required; audio has to land somewhere")
		}
		if cfg.MaxChars <= 0 {
			cfg.MaxChars = DefaultSpeakMaxChars
		}
		handlers = append(handlers, compute.FailoverHandler{Label: cfg.Label, Tier: cfg.TrustTier, Fn: newSpeakHandler(cfg)})
	}
	return b.Register("speak", compute.FailoverBuiltin("speak", nil, b.Health(), b.TrustFloor(), handlers...))
}

func newSpeakHandler(cfg SpeakConfig) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		text := strings.TrimSpace(args["text"])
		if text == "" {
			return nil, 2, errors.New("speak: text is required")
		}
		if len(text) > cfg.MaxChars {
			// Exit code 2: the model gave bad input and can fix it by
			// asking for less. Truncating silently would bill for audio
			// that stops mid-sentence.
			return nil, 2, fmt.Errorf("speak: text is %d characters, limit is %d — "+
				"synthesise a shorter passage", len(text), cfg.MaxChars)
		}

		var speed float32
		if s := strings.TrimSpace(args["speed"]); s != "" {
			v, err := strconv.ParseFloat(s, 32)
			if err != nil {
				return nil, 2, fmt.Errorf("speak: speed %q is not a number", s)
			}
			speed = float32(v)
		}

		started := time.Now()
		art, err := cfg.Driver.Speak(ctx, compute.SpeakRequest{
			Text:   text,
			Voice:  strings.TrimSpace(args["voice"]),
			Format: strings.TrimSpace(args["format"]),
			Speed:  speed,
		})
		// Billed per CHARACTER OF INPUT, which is what every TTS vendor
		// meters — not per second of output, which is not known until
		// the audio exists. Reported whether or not the call succeeded:
		// a failure costs nothing but is still worth a span.
		compute.CollectGeneration(ctx, cfg.Label, cfg.Model,
			compute.MeteredUsage(compute.UnitAudioCharacters, float64(len(text)), cfg.BilledTo),
			cfg.Pricing, started, err)
		if err != nil {
			// Returned unwrapped so the failover chain can read the
			// class the driver assigned.
			return nil, 1, err
		}

		got, err := cfg.Resolver.Resolve(ctx, art, speakFileName(text))
		if err != nil {
			return nil, 1, fmt.Errorf("speak: %w", err)
		}

		// Announce the file so the channel layer attaches it. Without
		// this the model gets a path and the user gets nothing —
		// audio the agent cannot hand over is audio it should not have
		// been billed for.
		compute.CollectArtifact(ctx, types.Attachment{
			Kind:      compute.AttachmentKindForMIME(got.MIME),
			MimeType:  got.MIME,
			Size:      int(got.Bytes),
			Reference: got.Mount + ":" + got.Path,
			Filename:  filepath.Base(got.Path),
		})

		// The PATH is the result, not the bytes. Audio cannot go into a
		// tool result the model reads; what the model needs is a
		// reference it can hand to the channel layer to attach.
		out, _ := json.Marshal(map[string]any{
			"mount": got.Mount,
			"path":  got.Path,
			"mime":  got.MIME,
			"bytes": got.Bytes,
		})
		return out, 0, nil
	}
}

// speakFileName derives a readable name from the opening words, so a
// mount full of generated audio is browsable rather than a wall of
// identifiers.
func speakFileName(text string) string {
	return compute.ArtifactFileName(text, "speech")
}

// SpeakToolDef describes the tool to the model.
//
// It says what to do with the result, because the failure mode
// otherwise is the model reading a path back to the user as if it
// were the answer.
func SpeakToolDef() *types.ToolDef {
	return &types.ToolDef{
		Name:        "speak",
		Path:        compute.BuiltinScheme + "speak",
		Description: "Synthesise speech from text and save it as an audio file. Use when the user asks to hear something, asks for a voice note, or is in a context where audio is more useful than text. Returns the mount and path of the saved file — hand that path to the channel so it can be attached; do not read the path aloud to the user as though it were the answer. Keep passages short: synthesis is billed per character.",
		ParametersSchema: []byte(`{
			"type": "object",
			"properties": {
				"text": {"type": "string", "description": "The text to speak. Prefer a few sentences over a long document."},
				"voice": {"type": "string", "description": "Optional provider-specific voice name. Empty uses the configured default."},
				"format": {"type": "string", "description": "Optional container: mp3 (default), wav, opus, flac."},
				"speed": {"type": "string", "description": "Optional speed multiplier, e.g. \"1.0\". Empty uses the provider default."}
			},
			"required": ["text"],
			"additionalProperties": false
		}`),
		RiskTier: types.RiskCommunicating,
	}
}
