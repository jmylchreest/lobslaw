package binaries

import (
	"context"
	"log/slog"
	"os/exec"
)

type brewManager struct{}

func (brewManager) Name() string { return "brew" }

func (brewManager) Hosts(_ InstallSpec) []string {
	return []string{
		"formulae.brew.sh",
		"github.com",
		"objects.githubusercontent.com",
		"ghcr.io",
		"pkg-containers.githubusercontent.com",
		"raw.githubusercontent.com",
	}
}

func (brewManager) Available(_ context.Context) bool {
	_, err := exec.LookPath("brew")
	return err == nil
}

func (brewManager) Install(ctx context.Context, spec InstallSpec, runner ProcessRunner, log *slog.Logger) error {
	args := append([]string{"install", spec.Package}, spec.Args...)
	// brew runs as the calling user; sudo is wrong for brew and we
	// reject it deliberately rather than honouring spec.Sudo.
	return runManagerCmd(ctx, runner, log, "brew", false, args)
}
