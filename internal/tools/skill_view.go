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

// Progressive disclosure, levels 1 and 2.
//
// Level 0 is the index in the system prompt: every skill's name, one
// line of description, and the NAMES of its bundled documents. It
// costs O(skills) and stays that way as those documents grow, which is
// the whole point — a prompt that inlined every skill's instructions
// would spend most of a context window on capabilities the turn will
// not use.
//
// Level 1 is this: the agent has decided a skill is relevant and asks
// for its instructions. Level 2 is one named document from the bundle.
//
// READ-ONLY, AND THAT IS WHY IT IS CHEAP TO ALLOW. Viewing a skill
// runs nothing, so the risk tier is the one that reads rather than the
// one that acts — an operator who had to approve every documentation
// read would approve without reading, which is how the approvals that
// matter get clicked through.

// SkillDoc is what a skill can be asked for.
type SkillDoc interface {
	// Body returns the skill's prose instructions, and whether it has
	// any. A skill with no body is ordinary: many are a handler and a
	// description.
	Body(name string) (string, bool)
	// Reference returns one bundled document by its declared path.
	// The bool distinguishes "no such skill or document" from an empty
	// one.
	Reference(name, path string) (string, bool)
	// Has reports whether the skill exists at all, so a typo in the
	// name is answerable differently from a skill that ships no
	// documentation.
	Has(name string) bool
}

// SkillViewConfig wires the skill_view builtin.
type SkillViewConfig struct {
	Docs SkillDoc

	// MaxBytes bounds one document. A skill that bundles a reference
	// manual would otherwise fill the context window it was designed
	// to protect, and the truncation is announced so the agent knows
	// it is reading a fragment rather than a short document.
	MaxBytes int
}

// DefaultSkillDocMaxBytes is generous for instructions and small
// enough that one document cannot dominate a turn.
const DefaultSkillDocMaxBytes = 32 << 10

// RegisterSkillViewBuiltin installs skill_view.
func RegisterSkillViewBuiltin(b *Builtins, cfg SkillViewConfig) error {
	if cfg.Docs == nil {
		return errors.New("skill_view: Docs required")
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultSkillDocMaxBytes
	}
	return b.Register("skill_view", newSkillViewHandler(cfg))
}

func newSkillViewHandler(cfg SkillViewConfig) compute.BuiltinFunc {
	return func(_ context.Context, args map[string]string) ([]byte, int, error) {
		name := strings.TrimSpace(args["name"])
		if name == "" {
			return nil, 2, errors.New("skill_view: name is required")
		}
		if !cfg.Docs.Has(name) {
			// Exit 2: the agent chose a name, and the fix is to choose
			// a different one from the index it already has.
			return nil, 2, fmt.Errorf("skill_view: no skill named %q is installed", name)
		}

		path := strings.TrimSpace(args["path"])
		if path == "" {
			body, ok := cfg.Docs.Body(name)
			if !ok {
				// NOT an error. A skill with no body is ordinary, and
				// reporting it as a failure would teach the agent to
				// avoid a tool that is working correctly.
				return skillViewResult(name, "", "",
					"this skill ships no instructions; its description in the index is all there is")
			}
			text, truncated := truncateDoc(body, cfg.MaxBytes)
			return skillViewResult(name, "", text, truncationNote(truncated))
		}

		ref, ok := cfg.Docs.Reference(name, path)
		if !ok {
			return nil, 2, fmt.Errorf(
				"skill_view: skill %q bundles no document at %q; the index lists the ones it has",
				name, path)
		}
		text, truncated := truncateDoc(ref, cfg.MaxBytes)
		return skillViewResult(name, path, text, truncationNote(truncated))
	}
}

func skillViewResult(name, path, content, note string) ([]byte, int, error) {
	out := map[string]any{"skill": name, "content": content}
	if path != "" {
		out["path"] = path
	}
	if note != "" {
		out["note"] = note
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, 1, err
	}
	return raw, 0, nil
}

// truncateDoc cuts at a rune boundary, because cutting mid-rune
// produces a replacement character the model reads as content.
func truncateDoc(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// truncationNote says so in words, because a document that stops
// mid-sentence otherwise reads as a document that ends there.
func truncationNote(truncated bool) string {
	if !truncated {
		return ""
	}
	return "truncated: this document is longer than one view; what is shown is the beginning"
}

// SkillViewToolDef is the ToolDef registered alongside the builtin.
func SkillViewToolDef() *types.ToolDef {
	return &types.ToolDef{
		Name:        "skill_view",
		Path:        compute.BuiltinScheme + "skill_view",
		Description: "Read a skill's full instructions, or one of its bundled reference documents. The system prompt lists every installed skill with a one-line description and the names of its documents; call this when you have decided a skill is relevant and need the detail. Pass name alone for the skill's own instructions, or name and path for one named document.",
		ParametersSchema: []byte(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "The skill's name, as listed in Installed Skills."},
				"path": {"type": "string", "description": "Optional. One of the skill's bundled document paths. Omit for the skill's own instructions."}
			},
			"required": ["name"],
			"additionalProperties": false
		}`),
		// Reversible: reading documentation acts on nothing. Anything
		// stricter would put an approval in front of the step that
		// decides whether an approval is needed at all.
		RiskTier: types.RiskReversible,
	}
}
