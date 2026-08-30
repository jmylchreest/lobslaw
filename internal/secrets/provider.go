// Package secrets resolves a secret reference through a configured
// backend, so a key can live in a vault rather than on every node's
// disk.
//
// Before this, pkg/config.ResolveSecret was the whole surface: env:VAR
// or file:/path, both resolving against the node's own machine. That
// made every provider key and channel token something provisioned per
// node, rotated by hand, and readable by anyone who could read the
// filesystem the sandbox exists to protect.
//
// The shape is the driver waist this codebase already applies to chat,
// vision, audio, image and search: one interface, a name→factory
// registry, and a wiring layer that assembles the set. Compiled drivers
// exist for backends whose failure modes are worth translating; `exec`
// covers everything else without any Go at all.
package secrets

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

// Provider fetches one secret from one backend.
//
// path is whatever follows the scheme in a reference: for "bw:app/key"
// the provider labelled "bw" is handed "app/key". Its meaning belongs
// entirely to the backend — an item path, a vault URI, an argument to a
// command — and this package does not parse it.
type Provider interface {
	Fetch(ctx context.Context, path string) (string, error)
}

// ProviderConfig is what every provider is built from.
type ProviderConfig struct {
	// Label is the reference scheme this provider answers to. Present
	// so a driver can name itself in its own errors, which is the
	// difference between "not signed in" and "provider \"op\" is not
	// signed in" when three vaults are configured.
	Label string

	// Command is the argv for the exec-family drivers. The compiled
	// vendor drivers default it and allow an override, because a
	// wrapper script around `bw` is a normal thing to have.
	Command []string

	// Env is the subprocess environment, already resolved and merged.
	// The wiring layer takes plaintext from config's env and resolves
	// config's secret_env through the BOOTSTRAP resolver only, so a
	// vault credential can never itself come from a vault.
	Env map[string]string

	// Timeout bounds one fetch. Zero takes DefaultFetchTimeout.
	Timeout time.Duration

	// Options are driver-specific scalars. map[string]string for the
	// same reason the search drivers use one: a new backend should not
	// need a config-struct change.
	Options map[string]string

	Logger *slog.Logger
}

// Factory builds one configured provider.
type Factory func(ProviderConfig) (Provider, error)

// Driver names. Config uses these strings.
const (
	// DriverExec runs a configured argv and takes its stdout. The
	// long tail — pass, gopass, sops, age, systemd-creds — and any
	// vendor CLI nobody has written a driver for.
	DriverExec = "exec"

	// DriverBitwarden and DriverOnePassword wrap `bw` and `op`. They
	// exist as compiled drivers rather than exec configs for one
	// reason: a locked vault or an expired session produces output
	// that means nothing on its own, and translating it into the
	// command that fixes it is worth a file.
	DriverBitwarden   = "bitwarden"
	DriverOnePassword = "onepassword"
)

// DefaultFetchTimeout bounds one fetch when config names none.
//
// Generous by the standards of this tree because a vault CLI may need
// to talk to a remote service, and the alternative to waiting is a node
// that will not boot.
const DefaultFetchTimeout = 15 * time.Second

// Registry maps a driver name to its factory.
//
// A value rather than a package-level table populated by init(), for
// the reason driverset.go gives: the set of available drivers should be
// visible at the call site rather than depending on which files happen
// to be imported.
type Registry struct {
	factories map[string]Factory
}

func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]Factory)}
}

// Register adds a driver under name.
func (r *Registry) Register(name string, f Factory) {
	if r.factories == nil {
		r.factories = make(map[string]Factory)
	}
	r.factories[normalise(name)] = f
}

// Build constructs the named driver.
func (r *Registry) Build(name string, cfg ProviderConfig) (Provider, error) {
	key := normalise(name)
	if key == "" {
		return nil, fmt.Errorf("secrets: provider %q declares no driver; available: %s",
			cfg.Label, strings.Join(r.Names(), ", "))
	}
	f, ok := r.factories[key]
	if !ok {
		return nil, fmt.Errorf("secrets: unknown driver %q for provider %q; available: %s",
			name, cfg.Label, strings.Join(r.Names(), ", "))
	}
	return f(cfg)
}

// Names lists the registered drivers, sorted, for error messages.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.factories))
	for k := range r.factories {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// DefaultRegistry is the set this binary ships with. Adding a driver is
// one file and one line here.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(DriverExec, ExecFactory)
	r.Register(DriverBitwarden, BitwardenFactory)
	r.Register(DriverOnePassword, OnePasswordFactory)
	return r
}

func normalise(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// option reads a driver option, trimmed.
func option(opts map[string]string, key string) string {
	return strings.TrimSpace(opts[key])
}

// unknownOptions reports keys a driver does not understand, so a typo
// is a boot error naming the key rather than a setting that parses and
// does nothing.
//
// Matched EXACTLY, because option() looks keys up exactly. The search
// drivers learned this the hard way: a case-folding validator paired
// with a case-sensitive reader lets `Method = "POST"` validate and then
// be silently ignored.
func unknownOptions(opts map[string]string, allowed ...string) []string {
	if len(opts) == 0 {
		return nil
	}
	known := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		known[a] = struct{}{}
	}
	var bad []string
	for k := range opts {
		if _, ok := known[k]; !ok {
			bad = append(bad, k)
		}
	}
	sort.Strings(bad)
	return bad
}

// FromConfig builds a resolver over every declared provider.
//
// Provider credentials resolve through Bootstrap and nothing else: a
// vault whose own key came from another vault would need one of them to
// work before either does.
func FromConfig(cfg config.SecretsConfig, reg *Registry, log *slog.Logger) (*Resolver, error) {
	if reg == nil {
		reg = DefaultRegistry()
	}
	built := make(map[string]Provider, len(cfg.Providers))
	for _, p := range cfg.Providers {
		label := normalise(p.Label)
		// Checked here as well as in Config.Validate, because this is
		// exported and takes a config struct a caller may not have
		// validated. A reserved label would BUILD fine and then be
		// unreachable — Resolve routes env: and file: to the bootstrap
		// path before it ever consults this map — and a duplicate would
		// silently take the last one. Both are the shape of failure
		// this package exists to stop shipping.
		if IsBootstrapScheme(label) {
			return nil, fmt.Errorf(
				"secrets: provider label %q is reserved; %s: resolves before any provider exists",
				p.Label, label)
		}
		if _, dup := built[label]; dup {
			return nil, fmt.Errorf("secrets: duplicate provider label %q; "+
				"a reference scheme can only mean one backend", p.Label)
		}
		// Plaintext first, then the resolved refs — so a secret_env
		// entry wins if both name the same variable, which is the only
		// ordering that cannot silently downgrade a secret to a
		// literal.
		env := make(map[string]string, len(p.Env)+len(p.SecretEnv))
		maps.Copy(env, p.Env)
		for k, ref := range p.SecretEnv {
			v, err := Bootstrap(ref)
			if err != nil {
				return nil, fmt.Errorf("secrets: provider %q: secret_env %s: %w", p.Label, k, err)
			}
			env[k] = v
		}
		provider, err := reg.Build(p.Driver, ProviderConfig{
			Label:   label,
			Command: p.Command,
			Env:     env,
			Timeout: p.Timeout,
			Options: p.Options,
			Logger:  log,
		})
		if err != nil {
			return nil, err
		}
		built[label] = provider
	}
	return NewResolver(built, cfg.CacheTTL), nil
}
