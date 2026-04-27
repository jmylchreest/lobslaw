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

	"github.com/jmylchreest/lobslaw/internal/sandbox"
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

// RunSpec bundles everything the runner needs for one invocation.
// A struct rather than a long argument list so extending the
// contract (adding cgroup quotas, rlimits, etc.) doesn't cascade
// through every test fake.
type RunSpec struct {
	Argv   []string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Policy *sandbox.Policy
}

// SubprocessRunner abstracts exec.Cmd creation so tests can
// substitute a fake that records argv / env / stdin / policy
// without actually spawning python/bash. Production uses CmdBuilder.
type SubprocessRunner interface {
	Run(ctx context.Context, spec RunSpec) (int, error)
}

// CmdBuilder is the production runner — builds an exec.Cmd,
// wires stdio, applies the sandbox policy, and returns the exit
// code.
type CmdBuilder struct{}

// Run spawns the subprocess and waits. Non-zero exit is NOT an
// error — the int exit code is the signal; err is reserved for
// spawn failures (binary missing, permission denied, sandbox
// install error).
func (CmdBuilder) Run(ctx context.Context, spec RunSpec) (int, error) {
	if len(spec.Argv) == 0 {
		return -1, errors.New("skills: empty argv")
	}
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Env = spec.Env
	cmd.Stdin = spec.Stdin
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr

	if spec.Policy != nil {
		if err := sandbox.Apply(cmd, spec.Policy); err != nil {
			return -1, fmt.Errorf("skills: sandbox apply: %w", err)
		}
	}

	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

// MountResolver returns the per-label rwx mode + resolved root for
// the active storage mounts. The skill invoker uses this to enforce
// "skill can't claim more than mount grants" at boot. Defining the
// shape here as an interface keeps internal/skills free of the
// internal/compute dependency that would otherwise be a cycle.
type MountResolver interface {
	ModeForLabel(label string) (root string, read, write, exec bool, ok bool)
}

// Invoker owns the registry + storage manager + runner. The agent
// (and tests) call Invoke; the invoker looks up the skill, resolves
// storage labels to paths, builds argv, and dispatches the
// subprocess under the injected runner.
type Invoker struct {
	reg     *Registry
	storage *storage.Manager
	mounts  MountResolver
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
	Mounts         MountResolver    // optional; required when manifest declares storage
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
		mounts:         cfg.Mounts,
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
	policy, err := buildPolicy(skill, argv[0], i.storage, i.mounts)
	if err != nil {
		return nil, err
	}

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
	exitCode, err := i.runner.Run(runCtx, RunSpec{
		Argv:   argv,
		Env:    env,
		Stdin:  bytesReader(stdinBytes),
		Stdout: stdout,
		Stderr: stderr,
		Policy: policy,
	})
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

// buildPolicy composes a per-invocation sandbox.Policy from the
// manifest. Rules:
//
//   - NoNewPrivs + default seccomp via Normalise once any
//     enforcement is requested (Landlock paths, at minimum).
//   - Namespaces: user + pid + ipc + uts (mount and network NOT
//     enabled yet — mount namespace would require a pivot_root
//     dance that's out of scope; network is policy-level, handled
//     separately when we wire egress rules).
//   - AllowedPaths: the manifest directory (so the interpreter can
//     read the handler script), /tmp (skill runtime scratch), and
//     one entry per declared storage access resolved via the
//     Manager. ReadOnlyPaths lists everything except write-mode
//     storage entries.
//   - Runtime interpreter path is admitted via AllowedPaths so
//     the subprocess can load libraries next to the binary (e.g.
//     Python's sys.path from /usr/lib/python3*).
//
// Returns nil when the skill declares no sandboxing signals AND
// there's no storage access — equivalent to "run as tools do with
// no policy." In practice we always produce a non-nil policy
// because we always want NoNewPrivs + default seccomp for skills.
func buildPolicy(skill *Skill, runtimePath string, mgr *storage.Manager, mounts MountResolver) (*sandbox.Policy, error) {
	policy := &sandbox.Policy{
		Namespaces: sandbox.NamespaceSet{
			User: true, PID: true, IPC: true, UTS: true,
		},
		NoNewPrivs: true,
	}

	// Handler directory — interpreter needs read+exec to load the
	// handler script and exec the bytecode it compiles.
	if filepath.IsAbs(skill.ManifestDir) {
		policy.Mounts = append(policy.Mounts, sandbox.PolicyMount{
			Path: skill.ManifestDir, Read: true, Exec: true,
		})
	}

	// Interpreter binary directory — read+exec so python3/bash + its
	// stdlib can load.
	if filepath.IsAbs(runtimePath) {
		policy.Mounts = append(policy.Mounts, sandbox.PolicyMount{
			Path: filepath.Dir(runtimePath), Read: true, Exec: true,
		})
	}

	// /tmp: scratch (bytecode caches, lockfiles, temp extractions).
	policy.Mounts = append(policy.Mounts, sandbox.PolicyMount{
		Path: "/tmp", Read: true, Write: true,
	})

	// Storage mounts. Skill manifest's StorageRead/StorageWrite
	// requested mode is intersected with the mount's actual rwx
	// ceiling — a skill claiming write against an "rx"-only mount
	// fails at policy build time, not silently downgraded.
	for _, s := range skill.Manifest.Storage {
		if mgr == nil {
			return nil, fmt.Errorf("skills: skill %q declares storage access but no Manager configured", skill.Name())
		}
		resolved, err := mgr.Resolve(s.Label)
		if err != nil {
			return nil, fmt.Errorf("skills: resolve storage %q: %w", s.Label, err)
		}
		want := sandbox.PolicyMount{Path: resolved, Read: true}
		if s.Mode == StorageWrite {
			want.Write = true
		}
		// When a MountResolver is wired we cap by mount mode;
		// otherwise (legacy / test) we accept the requested mode as-is.
		if mounts != nil {
			root, r, w, x, ok := mounts.ModeForLabel(s.Label)
			if !ok {
				return nil, fmt.Errorf("skills: skill %q references storage mount %q which is not registered", skill.Name(), s.Label)
			}
			if want.Read && !r {
				return nil, fmt.Errorf("skills: skill %q wants read on %q but mount has no read", skill.Name(), s.Label)
			}
			if want.Write && !w {
				return nil, fmt.Errorf("skills: skill %q wants write on %q but mount mode forbids it", skill.Name(), s.Label)
			}
			want.Path = root
			want.Exec = x // inherit exec from mount when granted; manifest doesn't yet model exec separately
		}
		policy.Mounts = append(policy.Mounts, want)
	}

	if err := policy.Validate(); err != nil {
		return nil, fmt.Errorf("skills: policy: %w", err)
	}
	return policy, nil
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
