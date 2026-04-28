package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/binaries"
	"github.com/jmylchreest/lobslaw/internal/clawhub"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// ClawhubConfig wires the clawhub_install builtin. Installer is the
// node-constructed clawhub.Installer; DefaultMount is the storage
// mount label bundles land under (typically "skill-tools"). Without
// an installer (operator hasn't enabled clawhub), the builtin is
// not registered.
type ClawhubConfig struct {
	Installer    *clawhub.Installer
	DefaultMount string

	// AutoEmitInstallRules, when true, makes a successful install
	// also write a policy rule allowing the agent to call the
	// newly-installed skill's tool. PolicyAdder is the seam — the
	// builtin calls it post-install with a synthesized PolicyRule.
	// Both must be set; either alone is a no-op. The operator's
	// [security] clawhub_auto_emit_install_rules toggle plumbs
	// through to AutoEmitInstallRules at wire time.
	AutoEmitInstallRules bool
	PolicyAdder          PolicyRuleAdder
	Logger               *slog.Logger
}

// PolicyRuleAdder is the subset of policy.Service the auto-emit path
// needs. Interface so tests can stub without standing up a raft FSM.
type PolicyRuleAdder interface {
	AddRule(ctx context.Context, req *lobslawv1.AddRuleRequest) (*lobslawv1.AddRuleResponse, error)
}

// RegisterClawhubBuiltin installs the clawhub_install handler. Empty
// installer is a soft skip — the agent simply doesn't see the tool
// and tells the user honestly that clawhub isn't configured.
func RegisterClawhubBuiltin(b *Builtins, cfg ClawhubConfig) error {
	if cfg.Installer == nil {
		return nil
	}
	if cfg.DefaultMount == "" {
		cfg.DefaultMount = "skill-tools"
	}
	return b.Register("clawhub_install", newClawhubInstallHandler(cfg))
}

// ClawhubToolDefs returns the tool registrations. Owner-only; the
// install path mutates the operator's storage mount and registers
// new tools on the next skill-registry scan, so default-deny via
// the wire_seeds.go noSeedBuiltins set.
func ClawhubToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "clawhub_install",
			Path:        BuiltinScheme + "clawhub_install",
			Description: "Install a skill from the configured clawhub catalogue. Two modes: (a) by slug — pass slug=\"<name>\" or slug=\"<owner>/<name>\" (e.g. \"gog\" or \"steipete/gog\") to install a clawhub.ai-format bundle; owner prefix is informational and stripped before API calls. The runtime parses SKILL.md, satisfies declared host bin requirements, and registers the synthetic skill. (b) by name+version — for native (manifest.yaml) bundles served by a lobslaw-format catalogue. Optional mount overrides the storage label and subpath overrides the per-skill directory. Returns the install path + skill name. The skill registry's filesystem watcher picks the new manifest up automatically. Owner-only; default-deny applies.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"slug":              {"type": "string", "description": "Clawhub slug (<owner>/<name>) for clawhub-format bundles. Mutually exclusive with name+version."},
					"name":              {"type": "string", "description": "Skill name (native-format catalogue). Used with version."},
					"version":           {"type": "string", "description": "Version (semver). Used with name."},
					"mount":             {"type": "string", "description": "Storage mount label (default: skill-tools)."},
					"subpath":           {"type": "string", "description": "Subdirectory under the mount (default: <name>)."},
					"bootstrap_managers": {"type": "string", "description": "Set to 'true' to auto-install missing-but-bootstrappable managers (brew, uvx) via their official curl-sh installer before retrying. Off by default. Ask the user before opting in — bootstrap downloads + executes upstream-published install scripts."}
				},
				"additionalProperties": false
			}`),
			RiskTier: types.RiskIrreversible,
		},
	}
}

func newClawhubInstallHandler(cfg ClawhubConfig) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		slug := strings.TrimSpace(args["slug"])
		name := strings.TrimSpace(args["name"])
		version := strings.TrimSpace(args["version"])
		if slug == "" && (name == "" || version == "") {
			return nil, 2, errors.New("clawhub_install: pass slug=<owner>/<name> for clawhub-format, or name+version for native-format")
		}
		if slug != "" && (name != "" || version != "") {
			return nil, 2, errors.New("clawhub_install: slug is mutually exclusive with name+version")
		}
		mount := strings.TrimSpace(args["mount"])
		if mount == "" {
			mount = cfg.DefaultMount
		}
		target := clawhub.InstallTarget{
			MountLabel: mount,
			Subpath:    strings.TrimSpace(args["subpath"]),
		}
		bootstrap := strings.EqualFold(strings.TrimSpace(args["bootstrap_managers"]), "true")
		installer := cfg.Installer
		if bootstrap {
			installer = installer.WithSatisfyOptions(binaries.SatisfyOptions{BootstrapMissingManagers: true})
		}
		var (
			res *clawhub.InstallResult
			err error
		)
		if slug != "" {
			res, err = installer.InstallBySlug(ctx, slug, target)
		} else {
			entry, gerr := installer.Client().GetSkill(ctx, name, version)
			if gerr != nil {
				return nil, 1, fmt.Errorf("clawhub_install: %w", gerr)
			}
			res, err = installer.Install(ctx, entry, target)
		}
		if err != nil {
			return nil, 1, fmt.Errorf("clawhub_install: %w", err)
		}

		emittedRule := ""
		if cfg.AutoEmitInstallRules && cfg.PolicyAdder != nil && res.Name != "" {
			ruleID := "auto-clawhub-" + res.Name
			_, addErr := cfg.PolicyAdder.AddRule(ctx, &lobslawv1.AddRuleRequest{
				Rule: &lobslawv1.PolicyRule{
					Id:       ruleID,
					Subject:  "scope:owner",
					Action:   "tool:exec",
					Resource: res.Name,
					Effect:   "allow",
					Priority: 20,
				},
			})
			if addErr != nil {
				if cfg.Logger != nil {
					cfg.Logger.Warn("clawhub_install: auto-emit policy rule failed",
						"skill", res.Name, "rule_id", ruleID, "err", addErr)
				}
			} else {
				emittedRule = ruleID
				if cfg.Logger != nil {
					cfg.Logger.Info("clawhub_install: auto-emitted policy rule",
						"skill", res.Name, "rule_id", ruleID)
				}
			}
		}

		out, _ := json.Marshal(map[string]any{
			"name":              res.Name,
			"version":           res.Version,
			"install_dir":       res.InstallDir,
			"manifest_path":     res.ManifestPath,
			"signed_by":         res.SignedBy,
			"auto_emitted_rule": emittedRule,
		})
		return out, 0, nil
	}
}
