package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	koanftoml "github.com/knadh/koanf/parsers/toml/v2"
	koanfenv "github.com/knadh/koanf/providers/env/v2"
	koanffile "github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

const (
	envPrefix     = "LOBSLAW__" // prefix + section separator collapsed; no trailing-underscore pitfall
	keyDelim      = "."
	envSectionSep = "__" // double underscore separates sections; single stays inside a key name
)

// LoadOptions controls how Load resolves its config source.
type LoadOptions struct {
	Path    string // explicit path; wins over all other sources
	SkipEnv bool   // disable env-var overrides (tests)
}

// Load reads lobslaw configuration in priority order (highest wins):
// opts.Path, $LOBSLAW_CONFIG, ./config.toml, $XDG_CONFIG_HOME/lobslaw/config.toml,
// $HOME/.config/lobslaw/config.toml. Missing file is OK — env-only is valid.
//
// System-wide paths like /etc/lobslaw/ are deliberately NOT in the
// fallback chain: lobslaw is container-first, and in containers the
// config root is the working directory or a mounted volume, not /etc.
// Dev workflows use the CWD-relative or XDG paths.
//
// Env-var overrides use double underscore (__) as the section
// separator and preserve single underscores inside keys:
//
//	LOBSLAW__MEMORY__RAFT_PORT=9999          → memory.raft_port
//	LOBSLAW__MEMORY__ENCRYPTION__KEY_REF=... → memory.encryption.key_ref
//
// The prefix is lowercased and stripped; what remains is split on
// __ into a hierarchy path.
func Load(opts LoadOptions) (*Config, error) {
	k := koanf.New(keyDelim)

	path, err := findConfigPath(opts.Path)
	if err != nil {
		return nil, err
	}
	if path != "" {
		if err := k.Load(koanffile.Provider(path), koanftoml.Parser()); err != nil {
			return nil, fmt.Errorf("%w: read %s: %w", types.ErrInvalidConfig, path, err)
		}
	}

	if !opts.SkipEnv {
		if err := k.Load(koanfenv.Provider(".", koanfenv.Opt{
			Prefix: envPrefix,
			TransformFunc: func(key, value string) (string, any) {
				key = strings.TrimPrefix(key, envPrefix)
				key = strings.ToLower(key)
				key = strings.ReplaceAll(key, envSectionSep, keyDelim)
				return key, value
			},
		}), nil); err != nil {
			return nil, fmt.Errorf("%w: env overlay: %w", types.ErrInvalidConfig, err)
		}
	}

	cfg := &Config{}
	if err := k.UnmarshalWithConf("", cfg, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return nil, fmt.Errorf("%w: unmarshal: %w", types.ErrInvalidConfig, err)
	}
	cfg.resolvedPath = path

	// LOBSLAW_PROVIDER_<LABEL>_<FIELD> env vars merge with the TOML-
	// sourced providers slice before validation, so operators can
	// declare providers entirely via env (container-first workflow)
	// or override individual fields on a TOML-declared provider.
	if !opts.SkipEnv {
		if err := applyEnvProviders(cfg); err != nil {
			return nil, fmt.Errorf("%w: env providers: %w", types.ErrInvalidConfig, err)
		}
	}

	if cfg.Security.BinaryInstallPrefix == "" {
		cfg.Security.BinaryInstallPrefix = "/lobslaw/usr/local"
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks required-key invariants that cross subsystem
// boundaries and must hold before any node starts. Subsystem-
// specific invariants (e.g. cert file existence, provider label
// uniqueness) are validated by their owning packages.
func (c *Config) Validate() error {
	if c.Memory.Enabled && c.Memory.Encryption.KeyRef == "" {
		return fmt.Errorf("%w: memory.enabled=true requires memory.encryption.key_ref (e.g. env:LOBSLAW_MEMORY_KEY)", types.ErrInvalidConfig)
	}
	if c.Memory.Enabled && !c.Storage.Enabled {
		return fmt.Errorf("%w: memory.enabled=true requires storage.enabled=true on the same node (snapshot-export targets resolve via local storage mounts)", types.ErrInvalidConfig)
	}
	if err := validateProviderBackups(c.Compute.Providers); err != nil {
		return err
	}
	if err := validateTrustTiers(c.Compute.Providers); err != nil {
		return err
	}
	if err := validateContextConfig(c.Compute.Context); err != nil {
		return err
	}
	if err := validateSearchProviders(c.Compute); err != nil {
		return err
	}
	if err := validateSecretProviders(c.Secrets); err != nil {
		return err
	}
	if err := validateQueueMode(c.Gateway.QueueMode); err != nil {
		return err
	}
	return nil
}

// validateTrustTiers rejects an out-of-range numeric trust_tier.
//
// Needed because a BARE TOML integer never reaches the type's own
// UnmarshalText: mapstructure converts int to int directly, so only
// the quoted and named forms get validated on the way in. An operator
// writing `trust_tier = 500` would otherwise load a value that fails
// every comparison, and find out later through an error about
// something else.
//
// Zero is refused with its own message. It is the value an omitted
// field takes, so somebody who wrote it explicitly meant a tier and
// got "unset" — and "0 is not a tier" is a far better thing to read
// than silence.
func validateTrustTiers(providers []ProviderConfig) error {
	for _, p := range providers {
		if p.TrustTier == types.TrustUnset {
			// Genuinely absent is fine: it only has to be declared once
			// a floor exists, and that is soul's check to make.
			continue
		}
		if !p.TrustTier.IsValid() {
			return fmt.Errorf(
				"%w: provider %q has trust_tier %d, which is out of range; use 1..%d "+
					"or a name (public, private, local), where higher is more trusted",
				types.ErrInvalidConfig, p.Label, int(p.TrustTier), int(types.MaxTrustTier))
		}
	}
	return nil
}

// validateSearchProviders rejects the search config that would
// otherwise go wrong quietly.
//
// Which driver names exist is deliberately NOT checked here: the
// registry lives in internal/compute, above this package, and it
// already produces a better error than a duplicated list could
// ("unknown search driver %q; available: ..."). This checks only what
// config alone can know.
func validateSearchProviders(c ComputeConfig) error {
	seen := make(map[string]struct{}, len(c.SearchProviders))
	for _, p := range c.SearchProviders {
		if p.Label == "" {
			return fmt.Errorf("%w: every compute.search_providers entry needs a label", types.ErrInvalidConfig)
		}
		if _, dup := seen[p.Label]; dup {
			return fmt.Errorf("%w: duplicate compute.search_providers label %q", types.ErrInvalidConfig, p.Label)
		}
		seen[p.Label] = struct{}{}
		if p.TrustTier != types.TrustUnset && !p.TrustTier.IsValid() {
			return fmt.Errorf(
				"%w: search provider %q has trust_tier %d, which is out of range; use 1..%d "+
					"or a name (public, private, local), where higher is more trusted",
				types.ErrInvalidConfig, p.Label, int(p.TrustTier), int(types.MaxTrustTier))
		}
	}

	// Several backends declared and none chosen is ambiguous, and the
	// two ways of guessing — first declared, or all of them as a chain
	// — are both defensible, which is the tell that the operator
	// should say. A single declared backend needs no such statement.
	if len(c.SearchProviders) > 1 && len(c.WebSearch.Providers) == 0 && c.WebSearch.Provider == "" {
		return fmt.Errorf(
			"%w: %d search providers declared but compute.web_search selects none; "+
				"set providers = [\"label\", ...] in preference order (a chain fails over in the order given)",
			types.ErrInvalidConfig, len(c.SearchProviders))
	}
	if len(c.WebSearch.Providers) > 0 && c.WebSearch.Provider != "" {
		return fmt.Errorf("%w: compute.web_search sets both provider and providers; providers alone expresses either case",
			types.ErrInvalidConfig)
	}
	return nil
}

// reservedSecretSchemes are the two references that resolve against
// this machine and must not be shadowed by a provider.
//
// Duplicated here rather than imported from internal/secrets because
// pkg/config sits below internal and must not depend on it — the same
// reason validateQueueMode duplicates the gateway's mode names. The
// agreement is asserted from internal/secrets, which CAN see this
// package.
var reservedSecretSchemes = []string{"env", "file"}

// ReservedSecretSchemes exposes that list so internal/secrets can
// assert the two agree rather than hope they do — the same move
// internal/gateway makes for the queue-mode names.
func ReservedSecretSchemes() []string {
	return append([]string(nil), reservedSecretSchemes...)
}

// validateSecretProviders rejects secret config that would otherwise
// fail confusingly at first use.
//
// Which driver names exist is deliberately NOT checked: the registry
// lives in internal/secrets, above this package, and it already
// produces a better message than a duplicated list could.
func validateSecretProviders(c SecretsConfig) error {
	seen := make(map[string]struct{}, len(c.Providers))
	for _, p := range c.Providers {
		label := strings.ToLower(strings.TrimSpace(p.Label))
		if label == "" {
			return fmt.Errorf("%w: every [[secrets.providers]] entry needs a label; "+
				"the label is the reference scheme, so a provider labelled \"bw\" makes \"bw:app/key\" resolvable",
				types.ErrInvalidConfig)
		}
		for _, r := range reservedSecretSchemes {
			if label == r {
				return fmt.Errorf(
					"%w: [[secrets.providers]] label %q is reserved; %s: resolves against this machine "+
						"before any provider exists, and shadowing it would make the bootstrap path "+
						"depend on the thing it bootstraps",
					types.ErrInvalidConfig, p.Label, r)
			}
		}
		if _, dup := seen[label]; dup {
			return fmt.Errorf("%w: duplicate [[secrets.providers]] label %q; "+
				"a reference scheme can only mean one backend", types.ErrInvalidConfig, p.Label)
		}
		seen[label] = struct{}{}

		if strings.TrimSpace(p.Driver) == "" {
			return fmt.Errorf("%w: [[secrets.providers]] %q needs a driver (exec, bitwarden or onepassword)",
				types.ErrInvalidConfig, p.Label)
		}
		for _, arg := range p.Command {
			if strings.TrimSpace(arg) == "" {
				return fmt.Errorf("%w: [[secrets.providers]] %q has an empty element in command; "+
					"an empty argument is passed to the process and is almost never meant",
					types.ErrInvalidConfig, p.Label)
			}
		}
		if p.Timeout < 0 {
			return fmt.Errorf("%w: [[secrets.providers]] %q has a negative timeout",
				types.ErrInvalidConfig, p.Label)
		}
		// env used to BE the reference field, and a config written
		// against that version would now hand its CLI the literal
		// string "env:BW_SESSION" as a session token — authentication
		// failing for a reason nothing in the error would name.
		//
		// A silent semantics change is the worst kind, so a value that
		// still looks like a reference is a boot error naming the field
		// that replaced it. A plaintext value legitimately beginning
		// "env:" or "file:" is not a thing anyone has.
		for k, v := range p.Env {
			for _, scheme := range reservedSecretSchemes {
				if strings.HasPrefix(strings.TrimSpace(v), scheme+":") {
					return fmt.Errorf(
						"%w: [[secrets.providers]] %q sets env.%s to %q, which is a secret reference — "+
							"env is plaintext, so move it to [secrets.providers.secret_env] "+
							"(env holds non-secret settings like a config directory or CA path)",
						types.ErrInvalidConfig, p.Label, k, v)
				}
			}
		}
	}
	return nil
}

// validateQueueMode rejects an unrecognised gateway.queue_mode at
// boot. The parser defaults anything unknown to "serial", which is
// the safe mode — but an operator who wrote "debounce_ms" or
// "Debounce " and got silent serialisation would have no way to tell
// their setting was ignored.
//
// The names are duplicated here rather than imported: pkg/config is
// below internal/gateway and must not depend on it.
//
// internal/gateway CAN see this package, so the agreement between the
// two lists is asserted from there rather than hoped for — a mode
// added to one and not the other fails at boot on a config the
// documentation says is valid, which is how "smart" first behaved.
func validateQueueMode(mode string) error {
	switch mode {
	case "", "serial", "latest", "debounce", "off", "smart":
		return nil
	default:
		return fmt.Errorf("%w: gateway.queue_mode = %q; want one of serial, latest, debounce, off, smart",
			types.ErrInvalidConfig, mode)
	}
}

// validateContextConfig rejects context-budget settings that would
// misbehave quietly rather than loudly. Every field here is optional,
// so only explicitly-set values are checked.
func validateContextConfig(c ContextConfig) error {
	for _, f := range []struct {
		name string
		val  *int
	}{
		{"tail_tokens", c.TailTokens},
		{"history_tool_result_bytes", c.HistoryToolResultBytes},
		{"compact_keep_messages", c.CompactKeepMessages},
		{"compact_trigger_tokens", c.CompactTriggerTokens},
		{"compact_max_summary_tokens", c.CompactMaxSummaryTokens},
		{"compact_tool_result_bytes", c.CompactToolResultBytes},
	} {
		if f.val != nil && *f.val < 0 {
			return fmt.Errorf("%w: compute.context.%s must not be negative (got %d)",
				types.ErrInvalidConfig, f.name, *f.val)
		}
	}
	// A summary larger than the whole verbatim budget means the
	// compacted head crowds out the recent exchange it was supposed
	// to make room for — the opposite of what compaction is for.
	if c.CompactMaxSummaryTokens != nil && c.TailTokens != nil &&
		*c.TailTokens > 0 && *c.CompactMaxSummaryTokens >= *c.TailTokens {
		return fmt.Errorf("%w: compute.context.compact_max_summary_tokens (%d) must be smaller than tail_tokens (%d), else the summary crowds out the conversation it summarises",
			types.ErrInvalidConfig, *c.CompactMaxSummaryTokens, *c.TailTokens)
	}
	return nil
}

// validateProviderBackups enforces two invariants on the implicit
// chain built from ProviderConfig.Backup pointers: every Backup
// value must reference an existing label, and walking the chain
// from any starting provider must terminate — no cycles.
func validateProviderBackups(providers []ProviderConfig) error {
	labels := make(map[string]bool, len(providers))
	for _, p := range providers {
		if p.Label == "" {
			continue
		}
		labels[p.Label] = true
	}
	for _, p := range providers {
		if p.Backup == "" {
			continue
		}
		if !labels[p.Backup] {
			return fmt.Errorf("%w: provider %q has backup=%q which is not a defined provider label", types.ErrInvalidConfig, p.Label, p.Backup)
		}
	}
	// Cycle detection: walk each starting point and bail if we
	// revisit. Bound the walk by the provider count to handle the
	// pathological case defensively.
	indexByLabel := make(map[string]int, len(providers))
	for i, p := range providers {
		indexByLabel[p.Label] = i
	}
	for _, start := range providers {
		if start.Backup == "" {
			continue
		}
		seen := map[string]bool{start.Label: true}
		cur := start.Backup
		for range providers {
			if seen[cur] {
				return fmt.Errorf("%w: provider backup chain has a cycle starting at %q (revisits %q)", types.ErrInvalidConfig, start.Label, cur)
			}
			seen[cur] = true
			idx, ok := indexByLabel[cur]
			if !ok {
				break
			}
			next := providers[idx].Backup
			if next == "" {
				break
			}
			cur = next
		}
	}
	return nil
}

func findConfigPath(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("%w: config file %q: %w", types.ErrInvalidConfig, explicit, err)
		}
		return explicit, nil
	}
	if p := os.Getenv("LOBSLAW_CONFIG"); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("%w: LOBSLAW_CONFIG=%q: %w", types.ErrInvalidConfig, p, err)
		}
		return p, nil
	}
	candidates := []string{"./config.toml"}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "lobslaw", "config.toml"))
	} else if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "lobslaw", "config.toml"))
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: stat %s: %w", types.ErrInvalidConfig, c, err)
		}
	}
	return "", nil
}
