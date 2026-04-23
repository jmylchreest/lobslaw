package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// envProviderPrefix is the namespace for env-declared providers.
// Shape: LOBSLAW_PROVIDER_<LABEL>_<FIELD>=value. The label part is
// matched CASE-INSENSITIVELY against config.toml's [[compute.providers]]
// label so operators writing shell-conventional UPPERCASE env vars
// can target lowercase labels in their TOML.
const envProviderPrefix = "LOBSLAW_PROVIDER_"

// supportedEnvFields are the ProviderConfig fields an operator may
// set via env vars. Ordered to make the docstring + error messages
// read predictably; order doesn't matter for matching.
//
// Pricing is deliberately excluded — it's a nested struct with
// three float fields and the resulting env-var names would be
// fiddly. Operators needing pricing overrides set it in config.toml.
var supportedEnvFields = map[string]struct{}{
	"ENDPOINT":     {},
	"MODEL":        {},
	"API_KEY":      {},
	"TRUST_TIER":   {},
	"CAPABILITIES": {},
}

// applyEnvProviders scans the process environment for
// LOBSLAW_PROVIDER_* entries, collects them into per-label
// ProviderConfig values, and merges with the existing Providers
// slice. Case-insensitive label matching; env overrides config.toml
// on per-field conflicts.
//
// Called from Load after koanf unmarshal + before Validate so the
// resulting Providers slice reflects both sources before validation
// fires.
func applyEnvProviders(cfg *Config) error {
	envProviders, err := collectEnvProviders(os.Environ())
	if err != nil {
		return err
	}
	cfg.Compute.Providers = mergeProviders(cfg.Compute.Providers, envProviders)
	return nil
}

// collectEnvProviders parses a slice of KEY=value strings (typically
// os.Environ()) and returns a map from normalised (lowercased) label
// to ProviderConfig. Each key contributes one field; all keys for a
// given label merge into one ProviderConfig.
//
// Returns an error if an env var looks like it's in our namespace
// but names an unknown field — prevents typos from silently being
// ignored.
func collectEnvProviders(env []string) (map[string]types.ProviderConfig, error) {
	out := make(map[string]types.ProviderConfig)
	capabilitiesByLabel := make(map[string][]string)

	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key := kv[:eq]
		val := kv[eq+1:]
		if !strings.HasPrefix(key, envProviderPrefix) {
			continue
		}
		rest := key[len(envProviderPrefix):]
		labelRaw, field, ok := splitLabelField(rest)
		if !ok {
			return nil, fmt.Errorf("env %q: malformed — expected LOBSLAW_PROVIDER_<LABEL>_<FIELD>", key)
		}
		if _, known := supportedEnvFields[field]; !known {
			return nil, fmt.Errorf("env %q: unknown field %q (supported: ENDPOINT, MODEL, API_KEY, TRUST_TIER, CAPABILITIES)", key, field)
		}

		labelKey := strings.ToLower(labelRaw)
		p := out[labelKey]
		if p.Label == "" {
			p.Label = labelKey
		}

		switch field {
		case "ENDPOINT":
			p.Endpoint = val
		case "MODEL":
			p.Model = val
		case "API_KEY":
			p.APIKeyRef = val
		case "TRUST_TIER":
			p.TrustTier = types.TrustTier(val)
		case "CAPABILITIES":
			capabilitiesByLabel[labelKey] = splitCommaTrim(val)
		}
		out[labelKey] = p
	}

	for labelKey, caps := range capabilitiesByLabel {
		p := out[labelKey]
		p.Capabilities = caps
		out[labelKey] = p
	}
	return out, nil
}

// splitLabelField splits "<LABEL>_<FIELD>" into its parts. The field
// is the LONGEST known suffix (ENDPOINT / MODEL / API_KEY /
// TRUST_TIER / CAPABILITIES) — this matters because some field
// names contain underscores (API_KEY, TRUST_TIER) and a naive
// split-on-last-underscore would mis-parse them.
func splitLabelField(rest string) (label, field string, ok bool) {
	// Check longest-first so "API_KEY" beats "KEY" etc.
	suffixes := []string{
		"_CAPABILITIES",
		"_TRUST_TIER",
		"_ENDPOINT",
		"_API_KEY",
		"_MODEL",
	}
	for _, s := range suffixes {
		if strings.HasSuffix(rest, s) {
			label = rest[:len(rest)-len(s)]
			field = strings.TrimPrefix(s, "_")
			if label == "" {
				return "", "", false
			}
			return label, field, true
		}
	}
	return "", "", false
}

// splitCommaTrim splits on "," and strips whitespace from each
// element. Empty strings (from consecutive commas or trailing
// comma) are filtered.
func splitCommaTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// mergeProviders combines config.toml-sourced providers with env-
// sourced providers. Env overrides config.toml per-field when the
// env value is non-empty. Labels match case-insensitively; the
// resulting Label field preserves the config.toml casing when a
// match exists (keeps operator-written labels stable) or uses the
// env-derived (lowercased) label when env-only.
func mergeProviders(configProviders []ProviderConfig, envProviders map[string]types.ProviderConfig) []ProviderConfig {
	envSeen := make(map[string]bool, len(envProviders))

	out := make([]ProviderConfig, 0, len(configProviders)+len(envProviders))
	for _, p := range configProviders {
		labelKey := strings.ToLower(p.Label)
		if env, ok := envProviders[labelKey]; ok {
			p = overlayProvider(p, env)
			envSeen[labelKey] = true
		}
		out = append(out, p)
	}
	for labelKey, env := range envProviders {
		if envSeen[labelKey] {
			continue
		}
		// Env-only provider — lift the types.ProviderConfig into the
		// config package's ProviderConfig shape. Capabilities +
		// TrustTier + base fields carry over directly.
		out = append(out, ProviderConfig{
			Label:        env.Label,
			Endpoint:     env.Endpoint,
			Model:        env.Model,
			APIKeyRef:    env.APIKeyRef,
			Capabilities: env.Capabilities,
			TrustTier:    env.TrustTier,
		})
	}
	return out
}

// overlayProvider returns base with any non-empty env field set to
// the env value. Zero-value env fields fall through — an operator
// who sets only LOBSLAW_PROVIDER_fast_MODEL doesn't wipe the
// endpoint.
func overlayProvider(base ProviderConfig, env types.ProviderConfig) ProviderConfig {
	if env.Endpoint != "" {
		base.Endpoint = env.Endpoint
	}
	if env.Model != "" {
		base.Model = env.Model
	}
	if env.APIKeyRef != "" {
		base.APIKeyRef = env.APIKeyRef
	}
	if env.TrustTier != "" {
		base.TrustTier = env.TrustTier
	}
	if len(env.Capabilities) > 0 {
		base.Capabilities = env.Capabilities
	}
	return base
}
