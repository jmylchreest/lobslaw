package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/clawhub"
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
			Description: "Install a skill from the configured clawhub catalog. Pass name (e.g. \"gws-workspace\") and version (semver). Optional mount overrides the default storage label and subpath overrides the per-skill subdirectory. Returns the install path + signer (when signature policy verified the bundle). The skill registry's filesystem watcher picks the new manifest up automatically; no node restart needed. Owner-only; default-deny applies.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"name":    {"type": "string", "description": "Skill name in the catalog."},
					"version": {"type": "string", "description": "Version (semver)."},
					"mount":   {"type": "string", "description": "Storage mount label (default: skill-tools)."},
					"subpath": {"type": "string", "description": "Subdirectory under the mount (default: <name>)."}
				},
				"required": ["name", "version"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskIrreversible,
		},
	}
}

func newClawhubInstallHandler(cfg ClawhubConfig) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		name := strings.TrimSpace(args["name"])
		version := strings.TrimSpace(args["version"])
		if name == "" || version == "" {
			return nil, 2, errors.New("clawhub_install: name and version are required")
		}
		entry, err := cfg.Installer.Client().GetSkill(ctx, name, version)
		if err != nil {
			return nil, 1, fmt.Errorf("clawhub_install: %w", err)
		}
		mount := strings.TrimSpace(args["mount"])
		if mount == "" {
			mount = cfg.DefaultMount
		}
		target := clawhub.InstallTarget{
			MountLabel: mount,
			Subpath:    strings.TrimSpace(args["subpath"]),
		}
		res, err := cfg.Installer.Install(ctx, entry, target)
		if err != nil {
			return nil, 1, fmt.Errorf("clawhub_install: %w", err)
		}
		out, _ := json.Marshal(map[string]any{
			"name":          res.Name,
			"version":       res.Version,
			"install_dir":   res.InstallDir,
			"manifest_path": res.ManifestPath,
			"signed_by":     res.SignedBy,
		})
		return out, 0, nil
	}
}
