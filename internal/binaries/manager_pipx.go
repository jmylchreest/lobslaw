package binaries

import (
	"context"
	"log/slog"
	"os/exec"
)

type pipxManager struct{}

func (pipxManager) Name() string { return "pipx" }

func (pipxManager) Hosts(_ InstallSpec) []string {
	return []string{
		"pypi.org",
		"files.pythonhosted.org",
	}
}

func (pipxManager) Available(_ context.Context) bool {
	_, err := exec.LookPath("pipx")
	return err == nil
}

func (pipxManager) Install(ctx context.Context, spec InstallSpec, runner ProcessRunner, log *slog.Logger) error {
	args := append([]string{"install", spec.Package}, spec.Args...)
	return runManagerCmd(ctx, runner, log, "pipx", false, args)
}

type uvxManager struct{}

// uvx isn't really an installer (it's a runner) but operators
// commonly want to install via `uv tool install`. Treat as user-mode
// install via uv.
func (uvxManager) Name() string { return "uvx" }

func (uvxManager) Hosts(_ InstallSpec) []string {
	return []string{"pypi.org", "files.pythonhosted.org"}
}

func (uvxManager) Available(_ context.Context) bool {
	_, err := exec.LookPath("uv")
	return err == nil
}

func (uvxManager) Install(ctx context.Context, spec InstallSpec, runner ProcessRunner, log *slog.Logger) error {
	args := append([]string{"tool", "install", spec.Package}, spec.Args...)
	return runManagerCmd(ctx, runner, log, "uv", false, args)
}

type npmManager struct{}

func (npmManager) Name() string { return "npm" }

func (npmManager) Hosts(_ InstallSpec) []string {
	return []string{"registry.npmjs.org"}
}

func (npmManager) Available(_ context.Context) bool {
	_, err := exec.LookPath("npm")
	return err == nil
}

func (npmManager) Install(ctx context.Context, spec InstallSpec, runner ProcessRunner, log *slog.Logger) error {
	args := append([]string{"install", "-g", spec.Package}, spec.Args...)
	return runManagerCmd(ctx, runner, log, "npm", false, args)
}

type cargoManager struct{}

func (cargoManager) Name() string { return "cargo" }

func (cargoManager) Hosts(_ InstallSpec) []string {
	return []string{"crates.io", "static.crates.io", "index.crates.io"}
}

func (cargoManager) Available(_ context.Context) bool {
	_, err := exec.LookPath("cargo")
	return err == nil
}

func (cargoManager) Install(ctx context.Context, spec InstallSpec, runner ProcessRunner, log *slog.Logger) error {
	args := append([]string{"install", spec.Package}, spec.Args...)
	return runManagerCmd(ctx, runner, log, "cargo", false, args)
}

type goInstallManager struct{}

func (goInstallManager) Name() string { return "go-install" }

func (goInstallManager) Hosts(_ InstallSpec) []string {
	return []string{"proxy.golang.org", "sum.golang.org"}
}

func (goInstallManager) Available(_ context.Context) bool {
	_, err := exec.LookPath("go")
	return err == nil
}

func (goInstallManager) Install(ctx context.Context, spec InstallSpec, runner ProcessRunner, log *slog.Logger) error {
	pkg := spec.Package
	args := append([]string{"install", pkg}, spec.Args...)
	return runManagerCmd(ctx, runner, log, "go", false, args)
}

type apkManager struct{}

func (apkManager) Name() string { return "apk" }

func (apkManager) Hosts(_ InstallSpec) []string {
	return []string{"dl-cdn.alpinelinux.org"}
}

func (apkManager) Available(_ context.Context) bool {
	_, err := exec.LookPath("apk")
	return err == nil
}

func (apkManager) Install(ctx context.Context, spec InstallSpec, runner ProcessRunner, log *slog.Logger) error {
	args := append([]string{"add", "--no-cache", spec.Package}, spec.Args...)
	return runManagerCmd(ctx, runner, log, "apk", true, args)
}
