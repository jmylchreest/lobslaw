// Package rclone implements the rclone-FUSE storage backend.
// Start spawns `rclone mount --daemon` with VFS cache; Stop signals
// the subprocess and runs `fusermount -u` to cleanly drop the
// mountpoint. Secrets in the mount config (cfg.Options: *_ref keys)
// are resolved via the injected SecretResolver before the
// subprocess is spawned so the process environment carries
// concrete values, not refs.
//
// Crypt (rclone's per-mount encryption layer) is documented as a
// follow-up — shipping the VFS-mount skeleton first so operators
// can start putting data on object stores without the extra
// operational complexity of crypt.
package rclone

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/storage"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// SecretResolver translates a "ref" string (e.g. env:AWS_KEY,
// file:/etc/secrets/bucket.key) into the concrete secret. Injected
// to keep this package free of a direct pkg/config dependency.
type SecretResolver func(ref string) (string, error)

// Runner abstracts subprocess execution. Production uses ExecRunner;
// tests inject FakeRunner to record invocations without spawning
// rclone or fusermount.
type Runner interface {
	Run(ctx context.Context, name string, env []string, args ...string) ([]byte, error)
}

// ExecRunner wraps os/exec for production use.
type ExecRunner struct{}

// Run executes name with args + env. CombinedOutput surfaces both
// stdout and stderr in error messages so operators see rclone's
// complaint in-context.
func (ExecRunner) Run(ctx context.Context, name string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w (output: %s)", name, err, string(out))
	}
	return out, nil
}

// Config bundles rclone-specific parameters. The rclone CLI expects
// `<remote>:<bucket>[/<path>]`; Remote + Bucket produce the source
// arg, Path (optional) appends after the bucket separator.
type Config struct {
	Label           string
	Remote          string            // rclone remote name (e.g. "s3-prod")
	Bucket          string            // object-store bucket (optional for local-style remotes)
	Path            string            // sub-path within the bucket (optional)
	MountRoot       string            // "/cluster/store" default
	VFSCacheMode    string            // "off" | "minimal" | "writes" | "full"; default "full"
	VFSCachePoll    time.Duration     // poll interval for the VFS cache
	Options         map[string]string // arbitrary rclone flags (key→value); "" value = bare flag
	SecretRefs      map[string]string // env-var name → secret ref (e.g. RCLONE_CONFIG_S3_PROD_ACCESS_KEY_ID → env:AWS_KEY)
	SecretResolver  SecretResolver    // required when SecretRefs non-empty
}

// Mount is the rclone-backend implementation of storage.Mount.
// Tracks the rclone daemon's PID so Stop can signal it cleanly.
type Mount struct {
	cfg    Config
	runner Runner

	mu        sync.Mutex
	started   bool
	pathCache string
}

// New constructs an rclone mount with the default runner.
func New(cfg Config) (*Mount, error) {
	return NewWithRunner(cfg, ExecRunner{})
}

// NewWithRunner is the testing entry point.
func NewWithRunner(cfg Config, runner Runner) (*Mount, error) {
	if cfg.Label == "" {
		return nil, errors.New("rclone: Label is required")
	}
	if cfg.Remote == "" {
		return nil, errors.New("rclone: Remote is required")
	}
	if cfg.MountRoot == "" {
		cfg.MountRoot = "/cluster/store"
	}
	if cfg.VFSCacheMode == "" {
		cfg.VFSCacheMode = "full"
	}
	if cfg.VFSCachePoll <= 0 {
		cfg.VFSCachePoll = time.Minute
	}
	if runner == nil {
		return nil, errors.New("rclone: Runner is required")
	}
	if len(cfg.SecretRefs) > 0 && cfg.SecretResolver == nil {
		return nil, errors.New("rclone: SecretRefs requires SecretResolver")
	}
	return &Mount{cfg: cfg, runner: runner}, nil
}

// Label returns the operator-chosen identifier.
func (m *Mount) Label() string { return m.cfg.Label }

// Backend returns "rclone" to match the StorageMount proto Type.
func (m *Mount) Backend() string { return "rclone" }

// Path returns the mountpoint. Empty until a successful Start.
func (m *Mount) Path() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pathCache
}

// Healthy reflects whether rclone's daemon launch returned cleanly.
// Doesn't prove the FUSE mount is currently responsive — callers
// who care Stat the mountpoint periodically.
func (m *Mount) Healthy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.started
}

// Start creates the mountpoint + resolves secrets + spawns rclone
// as a daemon. The --daemon flag detaches the process so the
// subprocess lifetime is independent of this Go goroutine;
// fusermount -u in Stop cleans it up. Failed secret resolution
// aborts before spawning.
func (m *Mount) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	mountpoint := filepath.Join(m.cfg.MountRoot, m.cfg.Label)
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return fmt.Errorf("rclone: mkdir %q: %w", mountpoint, err)
	}

	env, err := m.resolveEnv()
	if err != nil {
		return err
	}

	source := m.cfg.Remote + ":" + m.cfg.Bucket
	if m.cfg.Path != "" {
		source += "/" + strings.TrimPrefix(m.cfg.Path, "/")
	}

	args := []string{
		"mount",
		source,
		mountpoint,
		"--daemon",
		"--vfs-cache-mode", m.cfg.VFSCacheMode,
		"--vfs-cache-poll-interval", m.cfg.VFSCachePoll.String(),
	}
	for k, v := range m.cfg.Options {
		if v == "" {
			args = append(args, "--"+k)
		} else {
			args = append(args, "--"+k, v)
		}
	}

	if _, err := m.runner.Run(ctx, "rclone", env, args...); err != nil {
		return fmt.Errorf("rclone: %w", err)
	}
	m.started = true
	m.pathCache = mountpoint
	return nil
}

// Stop signals the rclone daemon via fusermount -u. A failing
// fusermount still marks the mount retired — the Manager needs it
// out of the set even if the FS layer is wedged, otherwise
// subsequent Register can't replace it.
func (m *Mount) Stop(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started {
		return nil
	}
	_, err := m.runner.Run(ctx, "fusermount", nil, "-u", m.pathCache)
	m.started = false
	m.pathCache = ""
	if err != nil {
		return fmt.Errorf("rclone: %w", err)
	}
	return nil
}

// resolveEnv walks SecretRefs, asks the SecretResolver for each,
// and returns a KEY=VALUE slice ready for exec.Cmd.Env. A
// resolution failure aborts — spawning rclone without a required
// secret just produces confusing runtime errors.
func (m *Mount) resolveEnv() ([]string, error) {
	if len(m.cfg.SecretRefs) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(m.cfg.SecretRefs))
	for name, ref := range m.cfg.SecretRefs {
		val, err := m.cfg.SecretResolver(ref)
		if err != nil {
			return nil, fmt.Errorf("rclone: resolve %q: %w", name, err)
		}
		out = append(out, name+"="+val)
	}
	return out, nil
}

// Factory is the storage.BackendFactory for the "rclone" type.
// Config.SecretResolver must be injected at factory-registration
// time (wrap Factory in a closure if the resolver varies per-node
// — see node.go's storage wiring).
func Factory(resolver SecretResolver) storage.BackendFactory {
	return func(cfg *lobslawv1.StorageMount) (storage.Mount, error) {
		if cfg == nil {
			return nil, errors.New("rclone: nil mount config")
		}
		options, secretRefs := splitOptionsAndSecrets(cfg.Options)
		return New(Config{
			Label:          cfg.Label,
			Remote:         cfg.Remote,
			Bucket:         cfg.Bucket,
			Path:           cfg.Path,
			Options:        options,
			SecretRefs:     secretRefs,
			SecretResolver: resolver,
		})
	}
}

// splitOptionsAndSecrets partitions the StorageMount Options map:
// keys ending in "_ref" are secret references (value resolved via
// SecretResolver at Start time); everything else is passed
// through to rclone as plain CLI flags. Convention mirrors the
// LLM-provider config where "api_key_ref" is a secret lookup
// rather than a literal key.
func splitOptionsAndSecrets(in map[string]string) (opts, refs map[string]string) {
	opts = make(map[string]string, len(in))
	refs = make(map[string]string)
	for k, v := range in {
		if strings.HasSuffix(k, "_ref") {
			// Normalise: strip "_ref" and uppercase — typical rclone
			// env var convention, e.g. "access_key_id_ref" →
			// RCLONE_CONFIG_<REMOTE>_ACCESS_KEY_ID is the rclone
			// convention; we expose the raw mapping so operators
			// can choose their own env-var scheme.
			envName := strings.ToUpper(strings.TrimSuffix(k, "_ref"))
			refs[envName] = v
			continue
		}
		opts[k] = v
	}
	return opts, refs
}
