package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// councilMaxFanout bounds how many providers can be queried in one
// council_review call. Each fan-out costs tokens and money; 4 is a
// reasonable ceiling for "second opinion" style questions. A higher
// ceiling is a footgun — the model can trivially fan out across
// every configured provider and burn budget.
const councilMaxFanout = 4

// CouncilConfig wires list_providers + council_review. The registry
// supplies label → client lookups; nil Registry disables both
// builtins.
type CouncilConfig struct {
	Registry *ProviderRegistry
}

// RegisterCouncilBuiltins installs list_providers + council_review
// when a Registry is supplied. Callers without a registry skip the
// wiring — tools won't appear in the LLM's function list.
func RegisterCouncilBuiltins(b *Builtins, cfg CouncilConfig) error {
	if cfg.Registry == nil {
		return errors.New("council builtins: Registry required")
	}
	if err := b.Register("list_providers", newListProvidersHandler(cfg.Registry)); err != nil {
		return err
	}
	return b.Register("council_review", newCouncilReviewHandler(cfg.Registry))
}

// CouncilToolDefs returns the ToolDef entries. Description copy
// emphasises provider labels (not model names) and makes the
// independent/adversarial distinction explicit.
func CouncilToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "list_providers",
			Path:        BuiltinScheme + "list_providers",
			Description: "List the LLM providers configured on this node. Returns label, trust tier, capabilities, and backup label for each — NO model names, NO endpoints, NO credentials. Use when the user asks who's available, or before council_review to pick providers. Present as a markdown table.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {},
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "council_review",
			Path:        BuiltinScheme + "council_review",
			Description: "Fan out a question to multiple providers in parallel, return their answers. Use when the user asks for 'a council', 'a second opinion', 'adversarial review', 'consensus check', or when you notice you're uncertain about a factual claim. question is required. providers is an optional array of provider labels (default: all configured). mode is 'independent' (each answers in isolation, default) or 'adversarial' (each sees the others' first-round answers and critiques/refines). Fan-out is capped at 4. Returns JSON {responses: [{label, content}], mode}. Narrate the results in your own voice — describe where providers agree, where they diverge, and which answer you lean toward (if any).",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"question": {"type": "string", "description": "The question to ask each provider."},
					"providers": {"type": "array", "items": {"type": "string"}, "description": "Optional provider labels (from list_providers). Default: all configured."},
					"mode": {"type": "string", "enum": ["independent", "adversarial"], "description": "'independent' (default): each answers alone. 'adversarial': round 2 includes other providers' answers for critique."}
				},
				"required": ["question"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskCommunicating,
		},
	}
}

func newListProvidersHandler(reg *ProviderRegistry) BuiltinFunc {
	return func(_ context.Context, _ map[string]string) ([]byte, int, error) {
		entries := reg.List()
		type view struct {
			Label        string   `json:"label"`
			TrustTier    string   `json:"trust_tier"`
			Capabilities []string `json:"capabilities,omitempty"`
			Backup       string   `json:"backup,omitempty"`
		}
		out := make([]view, 0, len(entries))
		for _, e := range entries {
			out = append(out, view{
				Label:        e.Label,
				TrustTier:    string(e.TrustTier),
				Capabilities: e.Capabilities,
				Backup:       e.Backup,
			})
		}
		payload, err := json.Marshal(map[string]any{
			"providers": out,
			"count":     len(out),
		})
		if err != nil {
			return nil, 1, err
		}
		return payload, 0, nil
	}
}

func newCouncilReviewHandler(reg *ProviderRegistry) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		question := strings.TrimSpace(args["question"])
		if question == "" {
			return nil, 2, errors.New("council_review: question is required")
		}
		mode := strings.TrimSpace(strings.ToLower(args["mode"]))
		if mode == "" {
			mode = "independent"
		}
		if mode != "independent" && mode != "adversarial" {
			return nil, 2, fmt.Errorf("council_review: mode must be 'independent' or 'adversarial', got %q", mode)
		}

		var providerLabels []string
		if raw, ok := args["providers"]; ok && raw != "" {
			if err := json.Unmarshal([]byte(raw), &providerLabels); err != nil {
				return nil, 2, fmt.Errorf("council_review: providers must be a JSON array of labels: %w", err)
			}
		}
		var targets []ProviderEntry
		if len(providerLabels) == 0 {
			targets = reg.List()
		} else {
			for _, label := range providerLabels {
				if e, ok := reg.Get(label); ok {
					targets = append(targets, e)
				}
			}
		}
		if len(targets) == 0 {
			return nil, 2, errors.New("council_review: no valid providers selected")
		}
		if len(targets) > councilMaxFanout {
			targets = targets[:councilMaxFanout]
		}

		// Round 1: fire question at every target in parallel.
		type response struct {
			Label   string `json:"label"`
			Content string `json:"content"`
			Error   string `json:"error,omitempty"`
		}
		responses := make([]response, len(targets))
		var wg sync.WaitGroup
		for i, t := range targets {
			wg.Add(1)
			go func(i int, t ProviderEntry) {
				defer wg.Done()
				responses[i] = response{Label: t.Label}
				roundCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
				defer cancel()
				req := ChatRequest{
					Messages: []Message{
						{Role: "user", Content: question},
					},
				}
				resp, err := t.Client.Chat(roundCtx, req)
				if err != nil {
					responses[i].Error = err.Error()
					return
				}
				responses[i].Content = stripReasoningTags(resp.Content)
			}(i, t)
		}
		wg.Wait()

		if mode == "adversarial" {
			// Round 2: each provider sees round-1 peer answers and
			// is asked to critique / refine. Serial inside per
			// provider — parallel across providers.
			var peerSummary strings.Builder
			peerSummary.WriteString("Round 1 answers from the council:\n\n")
			for _, r := range responses {
				fmt.Fprintf(&peerSummary, "--- %s ---\n%s\n\n", r.Label, firstNonEmpty(r.Content, r.Error))
			}
			round2 := make([]response, len(targets))
			var wg2 sync.WaitGroup
			for i, t := range targets {
				wg2.Add(1)
				go func(i int, t ProviderEntry) {
					defer wg2.Done()
					round2[i] = response{Label: t.Label}
					roundCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
					defer cancel()
					req := ChatRequest{
						Messages: []Message{
							{Role: "system", Content: "You are one of several providers answering the same question. Below are your peers' answers. Critique them, identify disagreements, and give your final refined answer. Be honest about uncertainty."},
							{Role: "user", Content: question + "\n\n" + peerSummary.String()},
						},
					}
					resp, err := t.Client.Chat(roundCtx, req)
					if err != nil {
						round2[i].Error = err.Error()
						return
					}
					round2[i].Content = stripReasoningTags(resp.Content)
				}(i, t)
			}
			wg2.Wait()
			responses = round2
		}

		payload, err := json.Marshal(map[string]any{
			"mode":      mode,
			"responses": responses,
		})
		if err != nil {
			return nil, 1, err
		}
		return payload, 0, nil
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
