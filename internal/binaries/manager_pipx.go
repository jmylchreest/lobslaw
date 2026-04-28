package binaries

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
)

func osEnviron() []string { return os.Environ() }

type pipxManager struct {
	prefix string
}

func (pipxManager) Name() string   { return "pipx" }
func (pipxManager) UserMode() bool { return true }

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

func (m pipxManager) Install(ctx context.Context, spec InstallSpec, runner ProcessRunner, log *slog.Logger) error {
	args := append([]string{"install", spec.Package}, spec.Args...)
	env := prefixEnv(m.prefix, map[string]string{
		"PIPX_HOME":    m.prefix + "/pipx",
		"PIPX_BIN_DIR": m.prefix + "/bin",
	})
	return runManagerCmdEnv(ctx, runner, log, "pipx", false, args, env)
}

type uvxManager struct {
	prefix string
}

// uvx isn't really an installer (it's a runner) but operators
// commonly want to install via `uv tool install`. Treat as user-mode
// install via uv.
func (uvxManager) Name() string   { return "uvx" }
func (uvxManager) UserMode() bool { return true }

func (uvxManager) Hosts(_ InstallSpec) []string {
	return []string{"pypi.org", "files.pythonhosted.org"}
}

func (uvxManager) Available(_ context.Context) bool {
	_, err := exec.LookPath("uv")
	return err == nil
}

func (m uvxManager) Install(ctx context.Context, spec InstallSpec, runner ProcessRunner, log *slog.Logger) error {
	args := append([]string{"tool", "install", spec.Package}, spec.Args...)
	env := prefixEnv(m.prefix, map[string]string{
		"UV_TOOL_BIN_DIR": m.prefix + "/bin",
		"UV_TOOL_DIR":     m.prefix + "/uv-tools",
	})
	return runManagerCmdEnv(ctx, runner, log, "uv", false, args, env)
}

type npmManager struct {
	prefix string
}

func (npmManager) Name() string   { return "npm" }
func (npmManager) UserMode() bool { return true }

func (npmManager) Hosts(_ InstallSpec) []string {
	return []string{"registry.npmjs.org"}
}

func (npmManager) Available(_ context.Context) bool {
	_, err := exec.LookPath("npm")
	return err == nil
}

func (m npmManager) Install(ctx context.Context, spec InstallSpec, runner ProcessRunner, log *slog.Logger) error {
	args := []string{"install", "-g"}
	if m.prefix != "" {
		args = append(args, "--prefix="+m.prefix)
	}
	args = append(args, spec.Package)
	args = append(args, spec.Args...)
	return runManagerCmd(ctx, runner, log, "npm", false, args)
}

type cargoManager struct {
	prefix string
}

func (cargoManager) Name() string   { return "cargo" }
func (cargoManager) UserMode() bool { return true }

func (cargoManager) Hosts(_ InstallSpec) []string {
	return []string{"crates.io", "static.crates.io", "index.crates.io"}
}

func (cargoManager) Available(_ context.Context) bool {
	_, err := exec.LookPath("cargo")
	return err == nil
}

func (m cargoManager) Install(ctx context.Context, spec InstallSpec, runner ProcessRunner, log *slog.Logger) error {
	args := []string{"install"}
	if m.prefix != "" {
		args = append(args, "--root="+m.prefix)
	}
	args = append(args, spec.Package)
	args = append(args, spec.Args...)
	return runManagerCmd(ctx, runner, log, "cargo", false, args)
}

type goInstallManager struct {
	prefix string
}

func (goInstallManager) Name() string   { return "go-install" }
func (goInstallManager) UserMode() bool { return true }

func (goInstallManager) Hosts(_ InstallSpec) []string {
	return []string{"proxy.golang.org", "sum.golang.org"}
}

func (goInstallManager) Available(_ context.Context) bool {
	_, err := exec.LookPath("go")
	return err == nil
}

func (m goInstallManager) Install(ctx context.Context, spec InstallSpec, runner ProcessRunner, log *slog.Logger) error {
	pkg := spec.Package
	args := append([]string{"install", pkg}, spec.Args...)
	env := prefixEnv(m.prefix, map[string]string{
		"GOBIN": m.prefix + "/bin",
	})
	return runManagerCmdEnv(ctx, runner, log, "go", false, args, env)
}

type apkManager struct{}

func (apkManager) Name() string   { return "apk" }
func (apkManager) UserMode() bool { return false }

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

// prefixEnv returns "KEY=VAL" entries appended to os.Environ. Returns
// nil when prefix is empty (callers pass nil → manager runs with the
// inherited env). Adds os.Environ first so we don't clobber inherited
// PATH/HOME.
func prefixEnv(prefix string, kv map[string]string) []string {
	if prefix == "" {
		return nil
	}
	env := append([]string(nil), osEnviron()...)
	for k, v := range kv {
		env = append(env, k+"="+v)
	}
	return env
}
