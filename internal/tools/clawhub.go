package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// ClawhubConfig exposes only proposal creation, never activation or policy writes.
// The node derives ownership and authorizes staging from the authenticated turn.
type ClawhubConfig struct {
	Propose func(context.Context, string) ([]byte, error)
}

func RegisterClawhubBuiltin(b *Builtins, cfg ClawhubConfig) error {
	if cfg.Propose == nil {
		return nil
	}
	return b.Register("clawhub_install", newClawhubInstallHandler(cfg))
}

func ClawhubToolDefs() []*types.ToolDef {
	return []*types.ToolDef{{Name: "clawhub_install", Path: compute.BuiltinScheme + "clawhub_install",
		Description:      "Fetch and stage a ClawHub skill proposal for the current user. Pass slug (name or owner/name), or name and version for a native catalogue. Returns an installation ID and human review instructions. Does not activate skills, install binaries or grant permissions. A human must review and activate through skills activate-install. Default-deny applies.",
		ParametersSchema: []byte(`{"type":"object","properties":{"slug":{"type":"string","description":"ClawHub slug; mutually exclusive with name and version."},"name":{"type":"string","description":"Native catalogue skill name."},"version":{"type":"string","description":"Native catalogue version, required with name."}},"additionalProperties":false}`),
		RiskTier:         types.RiskCommunicating}}
}

func newClawhubInstallHandler(cfg ClawhubConfig) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		for key := range args {
			if key != "slug" && key != "name" && key != "version" {
				return nil, 2, fmt.Errorf("clawhub_install: %q is not supported; this tool only stages proposals for human activation", key)
			}
		}
		slug, name, version := strings.TrimSpace(args["slug"]), strings.TrimSpace(args["name"]), strings.TrimSpace(args["version"])
		if slug == "" && (name == "" || version == "") {
			return nil, 2, errors.New("clawhub_install: supply slug or name and version")
		}
		if slug != "" && (name != "" || version != "") {
			return nil, 2, errors.New("clawhub_install: slug is mutually exclusive with name and version")
		}
		ref := "clawhub:" + slug
		if slug == "" {
			ref = "clawhub:" + name + "@" + version
		}
		if cfg.Propose == nil {
			return nil, 1, errors.New("clawhub_install: staging is unavailable")
		}
		raw, err := cfg.Propose(ctx, ref)
		if err != nil {
			return nil, 1, err
		}
		return raw, 0, nil
	}
}
