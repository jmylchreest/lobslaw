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

type BinariesConfig struct {
	Registry *binaries.Registry
}

func RegisterBinariesBuiltins(b *Builtins, cfg BinariesConfig) error {
	if cfg.Registry == nil {
		return nil
	}
	if err := b.Register("binary_install", newBinaryInstallHandler(cfg.Registry)); err != nil {
		return err
	}
	return b.Register("binary_list", newBinaryListHandler(cfg.Registry))
}

func BinariesToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "binary_list",
			Path:        BuiltinScheme + "binary_list",
			Description: "List operator-declared OS binaries the agent can install via binary_install. Each entry has name, manager (e.g. 'apt', 'brew', 'curl-sh'), host_support (whether this OS is supported), and installed (whether the binary is already present per the operator's detect command). Use before binary_install to confirm a binary is in the catalogue.",
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
			Description: "Install an OS-level binary the operator has declared in [[binary]]. The agent picks a name from binary_list and calls binary_install(name=...). The runtime picks an install method matching the host OS (apt, brew, pacman, dnf, apk, pipx, uvx, npm, cargo, go-install, or curl-sh-with-checksum). Idempotent — short-circuits if the operator's detect command reports the binary is already present. Owner-only; the trust gate is the [[binary]] catalogue itself (only declared binaries can be installed).",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"name": {"type": "string", "description": "Binary name as declared in [[binaries]] (e.g. 'gh', 'uvx', 'ffmpeg')."}
				},
				"required": ["name"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskIrreversible,
		},
	}
}

func newBinaryListHandler(reg *binaries.Registry) BuiltinFunc {
	return func(ctx context.Context, _ map[string]string) ([]byte, int, error) {
		entries := reg.List(ctx)
		out, _ := json.Marshal(map[string]any{"binaries": entries})
		return out, 0, nil
	}
}

func newBinaryInstallHandler(reg *binaries.Registry) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		name := strings.TrimSpace(args["name"])
		if name == "" {
			return nil, 1, errors.New("binary_install: name required")
		}
		result, err := reg.Install(ctx, name)
		if err != nil {
			return nil, 1, fmt.Errorf("binary_install: %w", err)
		}
		out, _ := json.Marshal(result)
		return out, 0, nil
	}
}
