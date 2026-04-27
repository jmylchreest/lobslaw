package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/soul"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// SoulMutator is the subset of *soul.Adjuster the builtins need.
// Defined as an interface so tests can swap in a stub without
// constructing a full Adjuster + on-disk file.
type SoulMutator interface {
	Soul() soul.Soul
	SetName(ctx context.Context, name string) (string, error)
	Tune(ctx context.Context, dimension string, delta int) (prev, next int, err error)
	SetEmojiUsage(ctx context.Context, value string) error
	AddFragment(ctx context.Context, text string) (cleaned string, total int, err error)
	RemoveFragment(ctx context.Context, needle string) (removed string, err error)
	ListFragments() []string
	HistoryRollback(ctx context.Context, steps int) (timestamp string, err error)
}

// SoulBuiltinsConfig wires the soul_* builtins. Mutator is required.
type SoulBuiltinsConfig struct {
	Mutator SoulMutator
}

// RegisterSoulBuiltins installs soul_get / soul_tune /
// soul_fragment_{add,remove,list} / soul_history_rollback.
//
// All mutator builtins are POLICY-DEFAULT-DENY at the seed layer
// (priority=10). Operators open per-scope (typically owner only)
// because the agent rewriting its own personality from a stranger's
// chat would be a serious foot-gun.
func RegisterSoulBuiltins(b *Builtins, cfg SoulBuiltinsConfig) error {
	if cfg.Mutator == nil {
		return errors.New("soul: Mutator required")
	}
	if err := b.Register("soul_get", soulGetHandler(cfg.Mutator)); err != nil {
		return err
	}
	if err := b.Register("soul_tune", soulTuneHandler(cfg.Mutator)); err != nil {
		return err
	}
	if err := b.Register("soul_fragment_add", soulFragmentAddHandler(cfg.Mutator)); err != nil {
		return err
	}
	if err := b.Register("soul_fragment_remove", soulFragmentRemoveHandler(cfg.Mutator)); err != nil {
		return err
	}
	if err := b.Register("soul_fragment_list", soulFragmentListHandler(cfg.Mutator)); err != nil {
		return err
	}
	if err := b.Register("soul_history_rollback", soulHistoryRollbackHandler(cfg.Mutator)); err != nil {
		return err
	}
	return nil
}

// SoulToolDefs is the tool def list registered alongside the
// builtins. Read tools (soul_get, soul_fragment_list) are tagged
// RiskRead; mutators are RiskCommunicating because they alter the
// agent's persistent identity.
func SoulToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:             "soul_get",
			Path:             BuiltinScheme + "soul_get",
			Description:      "Read the agent's current soul: name, persona description, emotive style dimensions (excitement/formality/directness/sarcasm/humor 0-10), emoji_usage, and the list of anecdotal fragments. Use this before tuning so you reason from the live state rather than guessing what's set.",
			ParametersSchema: []byte(`{"type":"object","properties":{},"additionalProperties":false}`),
			RiskTier:         types.RiskReversible,
		},
		{
			Name:        "soul_tune",
			Path:        BuiltinScheme + "soul_tune",
			Description: "Tune one bounded soul field. Use cases: rename (field=\"name\", value=\"Lobs\"), nudge an emotive dimension (field=\"sarcasm\", delta=1 or delta=-1 — capped to 0-10 and ±3 of baseline), or set emoji_usage (field=\"emoji_usage\", value=\"moderate\"). Pass EITHER value (string set) OR delta (int adjust) — not both. Cannot edit persona_description, body, scope, trust_tier, or any structural field — those are operator-only.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"field": {"type": "string", "enum": ["name", "excitement", "formality", "directness", "sarcasm", "humor", "emoji_usage"]},
					"value": {"type": "string", "description": "String value for name / emoji_usage."},
					"delta": {"type": "integer", "description": "Relative adjustment for the numeric emotive dimensions. Use +1 or -1 typically."}
				},
				"required": ["field"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskCommunicating,
		},
		{
			Name:        "soul_fragment_add",
			Path:        BuiltinScheme + "soul_fragment_add",
			Description: "Remember a short anecdotal fact about the user or yourself ('User supports Liverpool FC', 'Prefers Earl Grey tea brewed strong'). Capped at 200 chars per fragment; max 20 fragments total. Sanitised on write — control chars / backticks / code fences are stripped. Returns the cleaned text + the new total count. Use sparingly: each fragment becomes a bullet in your system prompt.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"text": {"type": "string", "description": "The fragment text. Short, declarative."}
				},
				"required": ["text"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskCommunicating,
		},
		{
			Name:        "soul_fragment_remove",
			Path:        BuiltinScheme + "soul_fragment_remove",
			Description: "Remove a fragment by substring match (case-insensitive). 'forget the liverpool thing' → call soul_fragment_remove(needle=\"liverpool\"). Returns the removed fragment.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"needle": {"type": "string", "description": "Substring to match against existing fragments."}
				},
				"required": ["needle"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskCommunicating,
		},
		{
			Name:             "soul_fragment_list",
			Path:             BuiltinScheme + "soul_fragment_list",
			Description:      "List all currently remembered anecdotal fragments. Useful when the user asks 'what do you remember about me?' or before pruning.",
			ParametersSchema: []byte(`{"type":"object","properties":{},"additionalProperties":false}`),
			RiskTier:         types.RiskReversible,
		},
		{
			Name:        "soul_history_rollback",
			Path:        BuiltinScheme + "soul_history_rollback",
			Description: "Undo a recent soul change. steps=1 reverts the most recent persist (last name/tune/fragment edit), steps=2 the one before, etc. Up to 20 versions are retained. Use when the user says 'undo your last personality change' or similar.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"steps": {"type": "integer", "minimum": 1, "maximum": 20, "description": "How many edits back to revert. Default 1."}
				},
				"required": [],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskCommunicating,
		},
	}
}

func soulGetHandler(m SoulMutator) BuiltinFunc {
	return func(_ context.Context, _ map[string]string) ([]byte, int, error) {
		s := m.Soul()
		out, _ := json.Marshal(map[string]any{
			"name":                s.Config.Name,
			"persona_description": s.Config.PersonaDescription,
			"emotive_style":       s.Config.EmotiveStyle,
			"fragments":           s.Config.Fragments,
		})
		return out, 0, nil
	}
}

func soulTuneHandler(m SoulMutator) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		field := strings.TrimSpace(args["field"])
		if field == "" {
			return nil, 2, errors.New("soul_tune: field required")
		}
		value := strings.TrimSpace(args["value"])
		rawDelta := strings.TrimSpace(args["delta"])

		switch field {
		case "name":
			if value == "" {
				return nil, 2, errors.New("soul_tune: value required for field=name")
			}
			cleaned, err := m.SetName(ctx, value)
			if err != nil {
				return nil, 2, err
			}
			out, _ := json.Marshal(map[string]any{"field": "name", "value": cleaned})
			return out, 0, nil

		case "emoji_usage":
			if value == "" {
				return nil, 2, errors.New("soul_tune: value required for field=emoji_usage")
			}
			if err := m.SetEmojiUsage(ctx, value); err != nil {
				return nil, 2, err
			}
			out, _ := json.Marshal(map[string]any{"field": "emoji_usage", "value": value})
			return out, 0, nil

		case "excitement", "formality", "directness", "sarcasm", "humor":
			if rawDelta == "" {
				return nil, 2, fmt.Errorf("soul_tune: delta required for field=%s", field)
			}
			delta, err := strconv.Atoi(rawDelta)
			if err != nil {
				return nil, 2, fmt.Errorf("soul_tune: delta must be integer, got %q", rawDelta)
			}
			prev, next, err := m.Tune(ctx, field, delta)
			if err != nil {
				out, _ := json.Marshal(map[string]any{
					"field": field, "prev": prev, "next": next, "applied": false, "reason": err.Error(),
				})
				return out, 0, nil
			}
			out, _ := json.Marshal(map[string]any{
				"field": field, "prev": prev, "next": next, "applied": true,
			})
			return out, 0, nil
		}
		return nil, 2, fmt.Errorf("soul_tune: unknown field %q", field)
	}
}

func soulFragmentAddHandler(m SoulMutator) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		text := strings.TrimSpace(args["text"])
		if text == "" {
			return nil, 2, errors.New("soul_fragment_add: text required")
		}
		cleaned, total, err := m.AddFragment(ctx, text)
		if err != nil {
			return nil, 2, err
		}
		out, _ := json.Marshal(map[string]any{"fragment": cleaned, "total": total})
		return out, 0, nil
	}
}

func soulFragmentRemoveHandler(m SoulMutator) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		needle := strings.TrimSpace(args["needle"])
		if needle == "" {
			return nil, 2, errors.New("soul_fragment_remove: needle required")
		}
		removed, err := m.RemoveFragment(ctx, needle)
		if err != nil {
			return nil, 2, err
		}
		out, _ := json.Marshal(map[string]any{"removed": removed})
		return out, 0, nil
	}
}

func soulFragmentListHandler(m SoulMutator) BuiltinFunc {
	return func(_ context.Context, _ map[string]string) ([]byte, int, error) {
		out, _ := json.Marshal(map[string]any{"fragments": m.ListFragments()})
		return out, 0, nil
	}
}

func soulHistoryRollbackHandler(m SoulMutator) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		steps := 1
		if raw := strings.TrimSpace(args["steps"]); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				return nil, 2, fmt.Errorf("soul_history_rollback: steps must be positive integer, got %q", raw)
			}
			steps = n
		}
		ts, err := m.HistoryRollback(ctx, steps)
		if err != nil {
			return nil, 1, err
		}
		out, _ := json.Marshal(map[string]any{"restored": ts, "steps": steps})
		return out, 0, nil
	}
}
