package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/jmylchreest/lobslaw/internal/storage"
)

// InvokeRequest is what the agent (or test) hands to the invoker.
// SkillName resolves via the Registry; Params is JSON-marshalled
// and piped to the subprocess on stdin.
type InvokeRequest struct {
	SkillName string
	Params    map[string]any
	Timeout   time.Duration
}

// InvokeResult is the subprocess outcome. Stdout is returned raw
// so callers can JSON-decode into a shape of their choosing;
// Stderr is captured for diagnostic surfacing.
type InvokeResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	Duration time.Duration
}

// SubprocessRunner abstracts exec.Cmd creation so tests can
// substitute a fake that records argv / env / stdin without
// actually spawning python/bash. Production uses CmdBuilder.
type SubprocessRunner interface {
	Run(ctx context.Context, argv []string, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)
}

// CmdBuilder is the production runner — builds an exec.Cmd,
// wires stdio, and returns the exit code.
type CmdBuilder struct{}

// Run spawns the subprocess and waits. Non-zero exit is NOT an
// error — the int exit code is the signal; err is reserved for
// spawn failures (binary missing, permission denied).
func (CmdBuilder) Run(ctx context.Context, argv []string, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(argv) == 0 {
		return -1, errors.New("skills: empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		// ExitError is a non-zero exit. Everything else is a real
		// spawn failure we should surface.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

// Invoker owns the registry + storage manager + runner. The agent
// (and tests) call Invoke; the invoker looks up the skill, resolves
// storage labels to paths, builds argv, and dispatches the
// subprocess under the injected runner.
type Invoker struct {
	reg     *Registry
	storage *storage.Manager
	runner  SubprocessRunner

	defaultTimeout time.Duration
}

// InvokerConfig bundles the dependencies. Storage is optional —
// when nil the invoker refuses to launch a skill that declares
// any storage access (fail-loud rather than fail-silent on a
// deployment that forgot to wire the storage manager).
type InvokerConfig struct {
	Registry       *Registry
	Storage        *storage.Manager
	Runner         SubprocessRunner // default: CmdBuilder{}
	DefaultTimeout time.Duration    // default: 30s
}

// NewInvoker constructs an invoker.
func NewInvoker(cfg InvokerConfig) (*Invoker, error) {
	if cfg.Registry == nil {
		return nil, errors.New("skills.Invoker: Registry required")
	}
	if cfg.Runner == nil {
		cfg.Runner = CmdBuilder{}
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = 30 * time.Second
	}
	return &Invoker{
		reg:            cfg.Registry,
		storage:        cfg.Storage,
		runner:         cfg.Runner,
		defaultTimeout: cfg.DefaultTimeout,
	}, nil
}

// Invoke runs a skill. The sandbox policy composition (Landlock
// rules computed from manifest.storage + runtime exec allowance,
// seccomp, namespaces) wraps the runner internally once the
// sandbox integration lands; today this is subprocess + stdio
// capture with path resolution.
func (i *Invoker) Invoke(ctx context.Context, req InvokeRequest) (*InvokeResult, error) {
	skill, err := i.reg.Get(req.SkillName)
	if err != nil {
		return nil, err
	}

	for _, s := range skill.Manifest.Storage {
		if i.storage == nil {
			return nil, fmt.Errorf("skills: skill %q declares storage access but no Manager configured", skill.Name())
		}
		if _, err := i.storage.Resolve(s.Label); err != nil {
			return nil, fmt.Errorf("skills: skill %q: resolve storage %q: %w", skill.Name(), s.Label, err)
		}
	}

	argv, err := buildArgv(skill)
	if err != nil {
		return nil, err
	}
	env := buildEnv(skill, i.storage)

	stdinBytes, err := json.Marshal(req.Params)
	if err != nil {
		return nil, fmt.Errorf("skills: marshal params: %w", err)
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = i.defaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout := &cappedBuffer{cap: 1 << 20}
	stderr := &cappedBuffer{cap: 64 << 10}

	started := time.Now()
	exitCode, err := i.runner.Run(runCtx, argv, env, bytesReader(stdinBytes), stdout, stderr)
	dur := time.Since(started)
	if err != nil {
		return nil, fmt.Errorf("skills: run %q: %w", skill.Name(), err)
	}
	return &InvokeResult{
		ExitCode: exitCode,
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
		Duration: dur,
	}, nil
}

// buildArgv turns the manifest runtime into a concrete argv. Each
// supported runtime maps to its interpreter + the handler script.
func buildArgv(skill *Skill) ([]string, error) {
	handler := skill.HandlerPath
	if !filepath.IsAbs(handler) {
		return nil, fmt.Errorf("skills: handler path not absolute: %q", handler)
	}
	switch skill.Manifest.Runtime {
	case RuntimePython:
		return []string{"python3", handler}, nil
	case RuntimeBash:
		return []string{"bash", handler}, nil
	default:
		return nil, fmt.Errorf("skills: unsupported runtime %q", skill.Manifest.Runtime)
	}
}

// buildEnv composes the environment the skill subprocess sees.
// Conventions:
//   - LOBSLAW_SKILL_NAME, LOBSLAW_SKILL_VERSION — always set so
//     handlers can log who they are.
//   - LOBSLAW_STORAGE_<LABEL> — one env var per declared storage
//     access, value is the resolved path. Lets bash handlers do
//     `cat "$LOBSLAW_STORAGE_SHARED/file.txt"` without re-parsing
//     config.
func buildEnv(skill *Skill, mgr *storage.Manager) []string {
	env := []string{
		"LOBSLAW_SKILL_NAME=" + skill.Name(),
		"LOBSLAW_SKILL_VERSION=" + skill.Manifest.Version,
	}
	for _, s := range skill.Manifest.Storage {
		if mgr == nil {
			continue
		}
		path, err := mgr.Resolve(s.Label)
		if err != nil {
			continue
		}
		env = append(env, fmt.Sprintf("LOBSLAW_STORAGE_%s=%s", toEnvVar(s.Label), path))
	}
	return env
}

// toEnvVar uppercases + replaces invalid env-var characters with
// underscore. Keeps env var names POSIX-safe without needing the
// operator to second-guess the label they picked.
func toEnvVar(label string) string {
	out := make([]byte, 0, len(label))
	for i := range len(label) {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z':
			out = append(out, c-('a'-'A'))
		case (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// cappedBuffer is a minimal io.Writer bounded by cap bytes. Extra
// writes past cap are silently discarded — the subprocess can't
// blow up the invoker's memory by flooding stdout.
type cappedBuffer struct {
	cap int
	buf []byte
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if len(c.buf) >= c.cap {
		return len(p), nil
	}
	room := c.cap - len(c.buf)
	if room > len(p) {
		room = len(p)
	}
	c.buf = append(c.buf, p[:room]...)
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte { return c.buf }

// bytesReader wraps a byte slice in an io.Reader. Avoids pulling
// in bytes.NewReader for a single use — keeps the package's
// transitive imports small.
func bytesReader(b []byte) io.Reader {
	return &sliceReader{b: b}
}

type sliceReader struct {
	b   []byte
	pos int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}
