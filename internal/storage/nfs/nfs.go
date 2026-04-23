// Package nfs implements the kernel-NFS storage backend. Start
// shells out to `mount -t nfs`; Stop to `umount`. Requires
// CAP_SYS_ADMIN or rootless-NFS capability on the runtime
// environment — documented in docs/dev/STORAGE.md as the primary
// operational constraint for this backend.
package nfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/jmylchreest/lobslaw/internal/storage"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Runner abstracts the subprocess invocation so tests can fake it.
// Production uses exec.Command wired via ExecRunner.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner is the production Runner — shells out to os/exec and
// returns combined stdout+stderr for diagnostic error reporting.
type ExecRunner struct{}

// Run executes the command and returns combined output. Non-zero
// exit becomes an error whose message includes both the underlying
// exec error and the captured output.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %v: %w (output: %s)", name, args, err, string(out))
	}
	return out, nil
}

// Config bundles the NFS-specific parameters pulled out of the
// replicated StorageMount. The mountpoint is derived from Label +
// MountRoot rather than exposed as config so operators can't
// collide with other mounts on the same node.
type Config struct {
	Label     string
	Server    string            // NFS server hostname or IP
	Export    string            // server-side export path
	MountRoot string            // local directory under which labels mount; default "/cluster/store"
	Options   map[string]string // e.g. nfsvers, sec; passed as -o
}

// Mount is the nfs-backend implementation of storage.Mount. The
// Runner is exported only via NewWithRunner for testability;
// production code uses New which wires ExecRunner.
type Mount struct {
	cfg    Config
	runner Runner

	mu        sync.Mutex
	started   bool
	pathCache string
}

// New constructs an NFS mount with the default (os/exec) runner.
// Fails fast on empty Label / Server / Export so misconfigs
// surface at the factory layer rather than at Start-time.
func New(cfg Config) (*Mount, error) {
	return NewWithRunner(cfg, ExecRunner{})
}

// NewWithRunner is the testing entry point — unit tests inject a
// FakeRunner that records the commands the mount would execute.
func NewWithRunner(cfg Config, runner Runner) (*Mount, error) {
	if cfg.Label == "" {
		return nil, errors.New("nfs: Label is required")
	}
	if cfg.Server == "" {
		return nil, errors.New("nfs: Server is required")
	}
	if cfg.Export == "" {
		return nil, errors.New("nfs: Export is required")
	}
	if cfg.MountRoot == "" {
		cfg.MountRoot = "/cluster/store"
	}
	if runner == nil {
		return nil, errors.New("nfs: Runner is required")
	}
	return &Mount{cfg: cfg, runner: runner}, nil
}

// Label returns the operator-chosen identifier.
func (m *Mount) Label() string { return m.cfg.Label }

// Backend returns "nfs" — matches the StorageMount proto's Type.
func (m *Mount) Backend() string { return "nfs" }

// Path returns the mountpoint. Empty until Start has completed
// successfully.
func (m *Mount) Path() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pathCache
}

// Healthy reflects the started flag. NFS doesn't expose per-mount
// liveness cheaply; callers that need richer health signals should
// Walk the mountpoint themselves.
func (m *Mount) Healthy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.started
}

// Start creates the mountpoint if it doesn't exist and invokes
// `mount -t nfs`. Options from cfg.Options are flattened into a
// comma-separated -o argument. Error from the mount command
// bubbles up untouched so operators see the kernel's complaint
// (stale handle, permission denied, etc).
func (m *Mount) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	mountpoint := filepath.Join(m.cfg.MountRoot, m.cfg.Label)
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return fmt.Errorf("nfs: mkdir %q: %w", mountpoint, err)
	}

	source := fmt.Sprintf("%s:%s", m.cfg.Server, m.cfg.Export)
	args := []string{"-t", "nfs"}
	if opts := flattenOptions(m.cfg.Options); opts != "" {
		args = append(args, "-o", opts)
	}
	args = append(args, source, mountpoint)

	if _, err := m.runner.Run(ctx, "mount", args...); err != nil {
		return fmt.Errorf("nfs: %w", err)
	}
	m.started = true
	m.pathCache = mountpoint
	return nil
}

// Stop runs `umount` against the mountpoint. A failed umount (e.g.
// a tool subprocess still holding a file) logs the error but still
// clears started — the manager needs the mount out of its set so a
// subsequent Register can replace it without colliding.
func (m *Mount) Stop(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.started {
		return nil
	}
	_, err := m.runner.Run(ctx, "umount", m.pathCache)
	m.started = false
	m.pathCache = ""
	if err != nil {
		return fmt.Errorf("nfs: %w", err)
	}
	return nil
}

// Factory is the storage.BackendFactory for the "nfs" type.
// Translates the replicated StorageMount into a Config by reading
// Server / Export directly and Options from the backend-specific
// map. MountRoot defaults to "/cluster/store" — operators who want
// a different prefix set NFS_MOUNT_ROOT in their env and wire a
// custom factory.
func Factory(cfg *lobslawv1.StorageMount) (storage.Mount, error) {
	if cfg == nil {
		return nil, errors.New("nfs: nil mount config")
	}
	return New(Config{
		Label:   cfg.Label,
		Server:  cfg.Server,
		Export:  cfg.Export,
		Options: cfg.Options,
	})
}

// flattenOptions renders the Options map as a deterministic
// comma-separated string for the `mount -o` argument. Keys sorted
// alphabetically so identical Options produces identical commands
// (tests can compare exact argv).
func flattenOptions(opts map[string]string) string {
	if len(opts) == 0 {
		return ""
	}
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sortStrings(keys)
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += ","
		}
		v := opts[k]
		if v == "" {
			out += k
		} else {
			out += k + "=" + v
		}
	}
	return out
}

// sortStrings is a tiny insertion sort — avoids pulling sort into
// a tight hot path for the typical 2-4 entry Options maps.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
