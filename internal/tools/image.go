package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// ImageConfig wires the generate_image builtin.
type ImageConfig struct {
	Driver   compute.ImageDriver
	Resolver *compute.ArtifactResolver

	// Label is the provider's config label, used as the health key so
	// a demotion is shared with every other modality that reaches the
	// same endpoint. Empty opts out of health tracking.
	Label string

	// TrustTier is the provider's declared tier, checked against the
	// soul's min_trust_tier before this provider is used. Empty fails
	// any set floor: an undeclared tier is not evidence of a high one.
	TrustTier types.TrustTier

	// Pricing is what this provider charges. Without it a generated
	// image costs the turn nothing, the spend cap cannot fire on it,
	// and the trace shows a free picture.
	Pricing types.ProviderPricing

	// Model is carried for the cost record, so an audit says which
	// model was billed rather than only which provider.
	Model string

	// BilledTo distinguishes a prepaid plan from a balance. A plan has
	// no marginal cost per call, so pricing it as though it did would
	// inflate every turn this provider served — the meaningful number
	// there is the quantity drawn against the quota.
	BilledTo compute.Billing

	// MaxPromptChars bounds one prompt. Image APIs reject over-long
	// prompts with a 400, which costs a round trip to learn something
	// checkable here. Zero picks DefaultImagePromptChars.
	MaxPromptChars int
}

// DefaultImagePromptChars matches the limit most image APIs impose.
const DefaultImagePromptChars = 4000

// RegisterImageBuiltin installs the generate_image tool. Variadic
// configs are a failover chain in priority order.
func RegisterImageBuiltin(b *Builtins, cfgs ...ImageConfig) error {
	if len(cfgs) == 0 {
		return errors.New("generate_image: at least one provider config required")
	}
	handlers := make([]compute.FailoverHandler, 0, len(cfgs))
	for _, cfg := range cfgs {
		if cfg.Driver == nil {
			return errors.New("generate_image: Driver required")
		}
		if cfg.Resolver == nil {
			return errors.New("generate_image: Resolver required; the image has to land somewhere")
		}
		if cfg.MaxPromptChars <= 0 {
			cfg.MaxPromptChars = DefaultImagePromptChars
		}
		handlers = append(handlers, compute.FailoverHandler{Label: cfg.Label, Tier: cfg.TrustTier, Fn: newImageHandler(cfg)})
	}
	return b.Register("generate_image", compute.FailoverBuiltin("generate_image", nil, b.Health(), b.TrustFloor(), handlers...))
}

func newImageHandler(cfg ImageConfig) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		prompt := strings.TrimSpace(args["prompt"])
		if prompt == "" {
			return nil, 2, errors.New("generate_image: prompt is required")
		}
		if len(prompt) > cfg.MaxPromptChars {
			return nil, 2, fmt.Errorf("generate_image: prompt is %d characters, limit is %d",
				len(prompt), cfg.MaxPromptChars)
		}

		started := time.Now()
		art, err := cfg.Driver.Generate(ctx, compute.ImageRequest{
			Prompt:  prompt,
			Size:    strings.TrimSpace(args["size"]),
			Quality: strings.TrimSpace(args["quality"]),
		})
		if err != nil {
			return nil, 1, err
		}

		// Billed per IMAGE, not per token. One request today; a
		// provider asked for variations would report the number it
		// actually returned.
		compute.CollectGeneration(ctx, cfg.Label, cfg.Model,
			compute.MeteredUsage(compute.UnitImages, 1, cfg.BilledTo), cfg.Pricing, started, nil)

		got, err := cfg.Resolver.Resolve(ctx, art, imageFileName(prompt))
		if err != nil {
			return nil, 1, fmt.Errorf("generate_image: %w", err)
		}

		compute.CollectArtifact(ctx, types.Attachment{
			Kind:      compute.AttachmentKindForMIME(got.MIME),
			MimeType:  got.MIME,
			Size:      int(got.Bytes),
			Reference: got.Mount + ":" + got.Path,
			Filename:  filepath.Base(got.Path),
		})

		out, _ := json.Marshal(map[string]any{
			"mount": got.Mount,
			"path":  got.Path,
			"mime":  got.MIME,
			"bytes": got.Bytes,
		})
		return out, 0, nil
	}
}

func imageFileName(prompt string) string {
	return compute.ArtifactFileName(prompt, "image")
}

// ImageToolDef describes the tool to the model. As with speak, it says
// what to do with the result: the return value is a path the channel
// attaches, not something to read out.
func ImageToolDef() *types.ToolDef {
	return &types.ToolDef{
		Name:        "generate_image",
		Path:        compute.BuiltinScheme + "generate_image",
		Description: "Generate an image from a text prompt and save it. Use when the user asks for a picture, diagram, illustration or mockup. Describe the subject, style and composition in the prompt — the model generating the image sees only this text, not the conversation. Returns the mount and path of the saved file for the channel to attach; do not read the path out to the user. Generation is billed per image, so do not call this speculatively.",
		ParametersSchema: []byte(`{
			"type": "object",
			"properties": {
				"prompt": {"type": "string", "description": "What to draw, including style and composition. Self-contained: the image model cannot see the conversation."},
				"size": {"type": "string", "description": "Optional, provider-specific, e.g. \"1024x1024\". Empty uses the configured default."},
				"quality": {"type": "string", "description": "Optional, provider-specific, e.g. \"standard\" or \"hd\"."}
			},
			"required": ["prompt"],
			"additionalProperties": false
		}`),
		RiskTier: types.RiskCommunicating,
	}
}
