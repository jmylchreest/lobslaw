package binaries

import (
	"context"
	"log/slog"
	"os/exec"
)

type dnfManager struct{}

func (dnfManager) Name() string   { return "dnf" }
func (dnfManager) UserMode() bool { return false }

func (dnfManager) Hosts(_ InstallSpec) []string {
	return []string{
		"mirrors.fedoraproject.org",
		"download.fedoraproject.org",
		"mirror.centos.org",
		"vault.centos.org",
	}
}

func (dnfManager) Available(_ context.Context) bool {
	if _, err := exec.LookPath("dnf"); err == nil {
		return true
	}
	_, err := exec.LookPath("yum")
	return err == nil
}

func (dnfManager) Install(ctx context.Context, spec InstallSpec, runner ProcessRunner, log *slog.Logger) error {
	bin := "dnf"
	if _, err := exec.LookPath("dnf"); err != nil {
		bin = "yum"
	}
	args := append([]string{"install", "-y", spec.Package}, spec.Args...)
	return runManagerCmd(ctx, runner, log, bin, true, args)
}
