package binaries

import (
	"context"
	"log/slog"
	"os/exec"
)

type pacmanManager struct{}

func (pacmanManager) Name() string   { return "pacman" }
func (pacmanManager) UserMode() bool { return false }

func (pacmanManager) Hosts(_ InstallSpec) []string {
	return []string{
		"archlinux.org",
		"mirror.archlinux.org",
		"geo.mirror.pkgbuild.com",
		"mirrors.kernel.org",
	}
}

func (pacmanManager) Available(_ context.Context) bool {
	_, err := exec.LookPath("pacman")
	return err == nil
}

func (pacmanManager) Install(ctx context.Context, spec InstallSpec, runner ProcessRunner, log *slog.Logger) error {
	args := append([]string{"-S", "--noconfirm", "--needed", spec.Package}, spec.Args...)
	return runManagerCmd(ctx, runner, log, "pacman", true, args)
}
