package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/clawhub"
	"github.com/jmylchreest/lobslaw/internal/storage"
	storagelocal "github.com/jmylchreest/lobslaw/internal/storage/local"
	"github.com/jmylchreest/lobslaw/pkg/config"
)

// pluginInstallClawhub is the CLI counterpart to the clawhub_install
// agent builtin. Source format: "clawhub:<name>@<version>". Resolves
// the catalog endpoint from the loaded config (or LOBSLAW_CLAWHUB_BASE_URL
// env), fetches the bundle, verifies SHA, extracts into rootOverride
// (or the default plugins root when empty).
func pluginInstallClawhub(source, rootOverride string) error {
	name, version, err := parseClawhubRef(source)
	if err != nil {
		return err
	}

	cfgPath := os.Getenv("LOBSLAW_CONFIG")
	cfg, err := config.Load(config.LoadOptions{Path: cfgPath})
	if err != nil && cfgPath != "" {
		return fmt.Errorf("load config %s: %w", cfgPath, err)
	}

	base := strings.TrimSpace(os.Getenv("LOBSLAW_CLAWHUB_BASE_URL"))
	if base == "" && cfg != nil {
		base = strings.TrimSpace(cfg.Security.ClawhubBaseURL)
	}
	if base == "" {
		return fmt.Errorf("clawhub base URL not configured (set [security].clawhub_base_url or $LOBSLAW_CLAWHUB_BASE_URL)")
	}

	dstRoot := rootOverride
	if dstRoot == "" {
		d, err := defaultSkillsRoot()
		if err != nil {
			return err
		}
		dstRoot = d
	}
	if err := os.MkdirAll(dstRoot, 0o755); err != nil {
		return fmt.Errorf("mkdir root: %w", err)
	}

	const mountLabel = "clawhub-cli-install"
	mgr := storage.NewManager()
	mount, err := storagelocal.New(storagelocal.Config{Label: mountLabel, Source: dstRoot})
	if err != nil {
		return fmt.Errorf("create mount: %w", err)
	}
	ctx := context.Background()
	if err := mgr.Register(ctx, mount); err != nil {
		return fmt.Errorf("register mount: %w", err)
	}
	defer func() { _ = mgr.Unregister(ctx, mountLabel) }()

	client, err := clawhub.NewClient(base)
	if err != nil {
		return fmt.Errorf("clawhub client: %w", err)
	}
	inst, err := clawhub.NewInstaller(clawhub.InstallerConfig{
		Client:  client,
		Storage: mgr,
		Policy:  clawhub.SigningOff,
	})
	if err != nil {
		return fmt.Errorf("clawhub installer: %w", err)
	}

	entry, err := client.GetSkill(ctx, name, version)
	if err != nil {
		return err
	}
	res, err := inst.Install(ctx, entry, clawhub.InstallTarget{
		MountLabel: mountLabel,
		Subpath:    name,
	})
	if err != nil {
		return err
	}
	fmt.Printf("installed %s@%s at %s\n", res.Name, res.Version, res.InstallDir)
	if res.SignedBy != "" {
		fmt.Printf("  signed by: %s\n", res.SignedBy)
	}
	return nil
}

// parseClawhubRef splits "clawhub:<name>@<version>" into its parts.
// The "@<version>" tail is required — there's no "latest" semantic
// (an unpinned install would let the catalog quietly upgrade us).
func parseClawhubRef(source string) (name, version string, err error) {
	tail := strings.TrimPrefix(source, "clawhub:")
	if tail == source {
		return "", "", fmt.Errorf("not a clawhub ref: %q (expected clawhub:<name>@<version>)", source)
	}
	at := strings.LastIndexByte(tail, '@')
	if at <= 0 || at == len(tail)-1 {
		return "", "", fmt.Errorf("clawhub ref %q must include @<version>", source)
	}
	name = strings.TrimSpace(tail[:at])
	version = strings.TrimSpace(tail[at+1:])
	if name == "" || version == "" {
		return "", "", fmt.Errorf("clawhub ref %q has empty name or version", source)
	}
	return name, version, nil
}
