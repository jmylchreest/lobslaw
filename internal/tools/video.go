package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Video is the first modality whose work outlives the turn. The tool
// cannot return the result, because the result does not exist yet:
// generation runs for minutes. So it submits, hands the handle to a
// commitment, and returns an acknowledgement. The scheduler polls, and
// the poll handler delivers.
//
// That split is the whole reason JobDriver exists, and it is why the
// tool description has to be explicit that the video arrives LATER —
// otherwise the model waits for something that will never come back
// in this turn, or tells the user it has failed.

// JobStarter records a submitted job so the scheduler can finish it.
// Injected rather than reaching for raft here, so this package stays
// free of the memory layer.
// JobStarter records a submitted job so something will collect it.
//
// providerLabel travels because the COST is not known until the job
// finishes, by which time the turn is long over — the only way to
// price seconds of video is to be able to find the provider's rate
// card again from the commitment.
type JobStarter func(ctx context.Context, h compute.JobHandle, providerLabel, prompt string) (commitmentID string, err error)

// VideoConfig wires the generate_video builtin.
type VideoConfig struct {
	Driver compute.JobDriver
	Start  JobStarter

	// MaxPromptChars bounds one prompt.
	MaxPromptChars int

	// Label is the provider's config label, carried onto the
	// commitment so the job's cost can be priced when it completes.
	Label string
}

// DefaultVideoPromptChars is generous: a video prompt carries scene,
// motion and style, so it runs longer than an image prompt.
const DefaultVideoPromptChars = 2000

// RegisterVideoBuiltin installs generate_video.
//
// Not variadic, unlike the synchronous modalities. Failover across
// providers does not belong at submit time for a job: the expensive,
// slow part happens after this returns, so a chain here would only
// cover the cheap submit call and would silently start work on two
// providers if the first submit succeeded but its response was lost.
// The commitment's own retry is the right place for that.
func RegisterVideoBuiltin(b *Builtins, cfg VideoConfig) error {
	if cfg.Driver == nil {
		return errors.New("generate_video: Driver required")
	}
	if cfg.Start == nil {
		return errors.New("generate_video: Start required; a submitted job with no commitment is lost work")
	}
	if cfg.MaxPromptChars <= 0 {
		cfg.MaxPromptChars = DefaultVideoPromptChars
	}
	return b.Register("generate_video", newVideoHandler(cfg))
}

func newVideoHandler(cfg VideoConfig) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		prompt := strings.TrimSpace(args["prompt"])
		if prompt == "" {
			return nil, 2, errors.New("generate_video: prompt is required")
		}
		if len(prompt) > cfg.MaxPromptChars {
			return nil, 2, fmt.Errorf("generate_video: prompt is %d characters, limit is %d",
				len(prompt), cfg.MaxPromptChars)
		}

		opts := map[string]string{}
		for _, k := range []string{"size", "duration"} {
			if v := strings.TrimSpace(args[k]); v != "" {
				opts[k] = v
			}
		}

		h, err := cfg.Driver.Submit(ctx, compute.JobRequest{
			Modality: compute.ModalityVideo,
			Prompt:   prompt,
			Options:  opts,
		})
		if err != nil {
			return nil, 1, err
		}

		// From here the job is running and being billed. A failure to
		// record the commitment means nothing will ever collect it, so
		// it is reported as an error rather than swallowed — an
		// operator seeing this knows to expect a charge for a video
		// nobody receives.
		id, err := cfg.Start(ctx, h, cfg.Label, prompt)
		if err != nil {
			return nil, 1, fmt.Errorf(
				"generate_video: job %s was submitted but could not be recorded, so its result "+
					"cannot be collected: %w", h.Raw, err)
		}

		out, _ := json.Marshal(map[string]any{
			"status":        "started",
			"commitment_id": id,
			"note":          "generation runs for several minutes; the video is delivered when it finishes",
		})
		return out, 0, nil
	}
}

// VideoToolDef describes the tool. The description leads with the
// asynchrony because that is the part the model gets wrong: without
// it, it reports the video as ready, or as failed, in the same turn.
func VideoToolDef() *types.ToolDef {
	return &types.ToolDef{
		Name:        "generate_video",
		Path:        compute.BuiltinScheme + "generate_video",
		Description: "Start generating a video from a text prompt. THE VIDEO IS NOT READY WHEN THIS RETURNS — generation takes several minutes, and the finished video is delivered to the user automatically when it completes. This tool returns only an acknowledgement that the job started. Tell the user it is being generated and that you will send it when ready; do not claim it is attached, do not wait for it, and do not call this again for the same request. Describe subject, motion and style in the prompt — the video model sees only this text, not the conversation. Video generation is expensive and billed per second, so never call it speculatively.",
		ParametersSchema: []byte(`{
			"type": "object",
			"properties": {
				"prompt": {"type": "string", "description": "What the video should show, including motion and style. Self-contained: the video model cannot see the conversation."},
				"size": {"type": "string", "description": "Optional, provider-specific, e.g. \"1280*720\"."},
				"duration": {"type": "string", "description": "Optional, provider-specific length in seconds."}
			},
			"required": ["prompt"],
			"additionalProperties": false
		}`),
		RiskTier: types.RiskCommunicating,
	}
}
