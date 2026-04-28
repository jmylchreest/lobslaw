package binaries

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"sync"
)

// Registry is the runtime binary catalogue. Holds the operator's
// declared binaries plus the per-manager implementations and
// resolves install requests against them.
type Registry struct {
	binaries map[string]Binary
	managers map[string]Manager
	runner   ProcessRunner
	log      *slog.Logger

	mu sync.RWMutex
}

// Config wires the registry. The HTTP client is used for the
// curl-sh manager; pass an egress-aware client (binaries-install
// role) so script downloads flow through smokescreen.
type Config struct {
	Binaries  []Binary
	HTTPClient *http.Client
	Runner     ProcessRunner
	Logger     *slog.Logger
}

// New builds a Registry. Returns an error if any operator binary
// fails Validate. Standard managers are always wired; the curl-sh
// manager is wired only when an HTTP client is supplied.
func New(cfg Config) (*Registry, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Runner == nil {
		cfg.Runner = ShellRunner{}
	}
	r := &Registry{
		binaries: make(map[string]Binary, len(cfg.Binaries)),
		managers: defaultManagers(cfg.HTTPClient),
		runner:   cfg.Runner,
		log:      cfg.Logger,
	}
	for _, b := range cfg.Binaries {
		if err := b.Validate(); err != nil {
			return nil, err
		}
		if _, dup := r.binaries[b.Name]; dup {
			return nil, fmt.Errorf("binary %q declared twice", b.Name)
		}
		r.binaries[b.Name] = b
	}
	return r, nil
}

func defaultManagers(client *http.Client) map[string]Manager {
	out := map[string]Manager{
		"apt":        aptManager{},
		"brew":       brewManager{},
		"pacman":     pacmanManager{},
		"dnf":        dnfManager{},
		"apk":        apkManager{},
		"pipx":       pipxManager{},
		"uvx":        uvxManager{},
		"npm":        npmManager{},
		"cargo":      cargoManager{},
		"go-install": goInstallManager{},
	}
	if client != nil {
		out["curl-sh"] = NewCurlShManager(client)
	}
	return out
}

// List returns the operator-declared binary catalogue, with detected
// install state populated.
func (r *Registry) List(ctx context.Context) []ListEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ListEntry, 0, len(r.binaries))
	for _, b := range r.binaries {
		entry := ListEntry{
			Name:        b.Name,
			Description: b.Description,
		}
		spec, ok := r.pickSpec(b)
		if ok {
			entry.Manager = spec.Manager
			entry.HostSupport = "supported"
		} else {
			entry.HostSupport = "no install spec for this OS/arch"
		}
		entry.Installed = r.detect(ctx, b)
		out = append(out, entry)
	}
	return out
}

// ListEntry is one row in the binary_list output.
type ListEntry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Manager     string `json:"manager,omitempty"`
	HostSupport string `json:"host_support"`
	Installed   bool   `json:"installed"`
}

// Get returns the operator declaration for a single binary, plus
// whether it's already installed on this host.
func (r *Registry) Get(ctx context.Context, name string) (Binary, bool, error) {
	r.mu.RLock()
	b, ok := r.binaries[name]
	r.mu.RUnlock()
	if !ok {
		return Binary{}, false, fmt.Errorf("binary %q not in catalogue", name)
	}
	return b, r.detect(ctx, b), nil
}

// Install resolves the binary, picks a host-matching install spec,
// short-circuits if the binary is already installed (per Detect),
// and runs the manager's Install. Returns nil iff the binary is
// installed by the time the call returns.
func (r *Registry) Install(ctx context.Context, name string) (InstallResult, error) {
	r.mu.RLock()
	b, ok := r.binaries[name]
	r.mu.RUnlock()
	if !ok {
		return InstallResult{}, fmt.Errorf("binary %q not in catalogue", name)
	}
	if r.detect(ctx, b) {
		return InstallResult{Name: name, AlreadyInstalled: true}, nil
	}

	spec, ok := r.pickSpec(b)
	if !ok {
		return InstallResult{}, fmt.Errorf("binary %q: no install spec matches this OS/arch", name)
	}

	r.mu.RLock()
	mgr, mgrOK := r.managers[spec.Manager]
	r.mu.RUnlock()
	if !mgrOK {
		return InstallResult{}, fmt.Errorf("manager %q not wired (curl-sh requires HTTP client)", spec.Manager)
	}
	if !mgr.Available(ctx) {
		return InstallResult{}, fmt.Errorf("%w: %s", errManagerNotAvailable, spec.Manager)
	}

	if err := mgr.Install(ctx, spec, r.runner, r.log); err != nil {
		return InstallResult{}, err
	}
	// Re-detect to confirm.
	if !r.detect(ctx, b) {
		return InstallResult{
			Name:    name,
			Manager: spec.Manager,
		}, errors.New("install command returned success but detect still reports missing — declared detect command may be wrong, or binary was installed to an unsearchable PATH")
	}
	return InstallResult{
		Name:    name,
		Manager: spec.Manager,
	}, nil
}

// InstallResult is the structured return from Install — fed into the
// builtin's JSON output.
type InstallResult struct {
	Name             string `json:"name"`
	Manager          string `json:"manager,omitempty"`
	AlreadyInstalled bool   `json:"already_installed,omitempty"`
}

// HostsFromBinaries returns the union of upstream host allowlists
// across the supplied binary declarations, used at boot to seed the
// "binaries-install" smokescreen role *before* the Registry itself
// is constructed (the egress provider is wired earlier than the
// registry, so it needs to compute hosts from raw config). Mirrors
// AllHosts() except a no-op manager (no HTTP client) is fine here —
// curl-sh hosts come from spec.URL, no installer state required.
func HostsFromBinaries(specs []Binary) []string {
	mgrs := defaultManagers(nil)
	mgrs["curl-sh"] = NewCurlShManager(nil)
	seen := make(map[string]struct{})
	var out []string
	for _, b := range specs {
		for _, spec := range b.Install {
			mgr, ok := mgrs[spec.Manager]
			if !ok {
				continue
			}
			for _, h := range mgr.Hosts(spec) {
				if h == "" {
					continue
				}
				if _, dup := seen[h]; dup {
					continue
				}
				seen[h] = struct{}{}
				out = append(out, h)
			}
		}
	}
	return out
}

// AllHosts returns the union of all host allowlists across declared
// binaries. Equivalent to HostsFromBinaries on the registry's own
// catalogue.
func (r *Registry) AllHosts() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[string]struct{})
	var out []string
	for _, b := range r.binaries {
		for _, spec := range b.Install {
			mgr, ok := r.managers[spec.Manager]
			if !ok {
				continue
			}
			for _, h := range mgr.Hosts(spec) {
				if _, dup := seen[h]; dup {
					continue
				}
				seen[h] = struct{}{}
				out = append(out, h)
			}
		}
	}
	return out
}

// Names returns every declared binary name. Used by the policy seed
// walker to know which binary_install:<name> resources exist.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.binaries))
	for name := range r.binaries {
		out = append(out, name)
	}
	return out
}

func (r *Registry) detect(ctx context.Context, b Binary) bool {
	if strings.TrimSpace(b.Detect) == "" {
		// No detect command means "always re-run install" — return
		// false so Install proceeds. The post-install detect is
		// bypassed for these (caller can see DetectMissing flag).
		return false
	}
	parts := strings.Fields(b.Detect)
	if len(parts) == 0 {
		return false
	}
	// Resolve the detect binary against PATH first; if it doesn't
	// exist, the binary's not installed and there's no point
	// running anything.
	if _, err := exec.LookPath(parts[0]); err != nil {
		return false
	}
	out, err := r.runner.Run(ctx, parts[0], parts[1:], nil)
	if err != nil {
		// Non-zero exit = not installed (or installed-but-broken;
		// we don't distinguish today).
		return false
	}
	_ = out
	return true
}

func (r *Registry) pickSpec(b Binary) (InstallSpec, bool) {
	for _, spec := range b.Install {
		if spec.Match() {
			return spec, true
		}
	}
	return InstallSpec{}, false
}
