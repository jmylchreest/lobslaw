package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/binaries"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// BinaryDeclaration is one operator-declared binary the agent can
// install via binary_install. Synthesized at boot from
// [[binary]] config blocks. PostInstall is the prose surfaced to
// the agent after a successful install — same shape as a clawhub
// SKILL.md prose body.
type BinaryDeclaration struct {
	Name        string
	Description string
	Detect      string
	Version     string
	Install     []binaries.InstallSpec
	PostInstall string
}

type BinariesConfig struct {
	Satisfier    *binaries.Satisfier
	Declarations map[string]BinaryDeclaration
}

func RegisterBinariesBuiltins(b *Builtins, cfg BinariesConfig) error {
	if cfg.Satisfier == nil || len(cfg.Declarations) == 0 {
		return nil
	}
	if err := b.Register("binary_install", newBinaryInstallHandler(cfg)); err != nil {
		return err
	}
	return b.Register("binary_list", newBinaryListHandler(cfg))
}

func BinariesToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "binary_list",
			Path:        BuiltinScheme + "binary_list",
			Description: "List operator-declared host binaries the agent can install via binary_install. Returns name, description, install methods, and whether the binary is already on PATH per the operator's detect command.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {},
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "binary_install",
			Path:        BuiltinScheme + "binary_install",
			Description: "Install a host binary the operator declared in [[binary]] config. Pass name (e.g. 'gog'). The runtime walks the declared install methods (gh-release, brew, apt, curl-sh, etc.) and picks the first one that works on this host. Idempotent — short-circuits if the binary is already on PATH per the operator's detect command. Returns the install result PLUS the operator's post_install prose, which contains setup instructions you should follow (e.g. 'gog auth credentials ...'). Owner-only; default-deny applies — operators allow specific binaries via [[policy.rules]] resource = 'binary_install:<name>'.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"name": {"type": "string", "description": "Binary name as declared in [[binary]] (e.g. 'gog')."},
					"bootstrap_managers": {"type": "string", "description": "Set to 'true' to auto-install missing-but-bootstrappable managers (brew, uvx) via their official curl-sh installer. Off by default."},
					"force": {"type": "string", "description": "Set to 'true' to reinstall even when the binary is already on PATH. Use for upgrades after the operator has bumped the version field or URL in [[binary]] config."}
				},
				"required": ["name"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskIrreversible,
		},
	}
}

func newBinaryListHandler(cfg BinariesConfig) BuiltinFunc {
	return func(ctx context.Context, _ map[string]string) ([]byte, int, error) {
		type entry struct {
			Name        string `json:"name"`
			Description string `json:"description,omitempty"`
			Installed   bool   `json:"installed"`
			Managers    []string `json:"managers,omitempty"`
		}
		out := make([]entry, 0, len(cfg.Declarations))
		for _, d := range cfg.Declarations {
			e := entry{Name: d.Name, Description: d.Description}
			e.Installed = cfg.Satisfier.Available(d.Name)
			seen := make(map[string]struct{})
			for _, s := range d.Install {
				if _, dup := seen[s.Manager]; dup {
					continue
				}
				seen[s.Manager] = struct{}{}
				e.Managers = append(e.Managers, s.Manager)
			}
			out = append(out, e)
		}
		body, _ := json.Marshal(map[string]any{"binaries": out})
		return body, 0, nil
	}
}

func newBinaryInstallHandler(cfg BinariesConfig) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		name := strings.TrimSpace(args["name"])
		if name == "" {
			return nil, 1, errors.New("binary_install: name required")
		}
		decl, ok := cfg.Declarations[name]
		if !ok {
			return nil, 1, fmt.Errorf("binary_install: %q is not declared in [[binary]] config", name)
		}
		bootstrap := strings.EqualFold(strings.TrimSpace(args["bootstrap_managers"]), "true")
		force := strings.EqualFold(strings.TrimSpace(args["force"]), "true")
		opts := binaries.SatisfyOptions{
			BootstrapMissingManagers: bootstrap,
			Force:                    force,
		}
		result, err := cfg.Satisfier.SatisfyOpts(ctx, decl.Name, decl.Install, opts)
		if err != nil {
			return nil, 1, fmt.Errorf("binary_install: %w", err)
		}
		body, _ := json.Marshal(map[string]any{
			"name":              result.Name,
			"manager":           result.Manager,
			"already_available": result.AlreadyAvailable,
			"post_install":      decl.PostInstall,
		})
		return body, 0, nil
	}
}
