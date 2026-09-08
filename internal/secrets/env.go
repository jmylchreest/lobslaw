package secrets

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// What a vault subprocess is allowed to see.
//
// Fetching one secret used to expose every other one. The subprocess
// was handed all of os.Environ(), and a lobslaw node's environment is
// where the provider API keys, the channel tokens and the memory
// encryption passphrase live — so `pass show lobslaw/openrouter` also
// handed `pass` the Anthropic key, the Telegram bot token and the Slack
// signing secret. The command is operator-configured rather than
// attacker-supplied, so this is a blast-radius problem rather than an
// exploit: any one vault CLI, wrapper script, or the wrong argv in
// config.toml sees everything.
//
// An allowlist rather than an empty environment, because the original
// reason for inheriting was correct and still is: `pass` reads
// GNUPGHOME and HOME, `op` reads its own config directory, and a
// provider started bare fails in ways that look like the vault is
// broken. The set below is "what a vault CLI needs to run at all" —
// locale, home, PATH, the XDG directories, and the sockets a pinentry
// or an agent is reached through.
//
// It holds no secret VALUES, which is not the same as holding nothing
// sensitive. SSH_AUTH_SOCK and DBUS_SESSION_BUS_ADDRESS are capability
// handles: the socket path is not a key, but a process that can reach
// the agent can ask it to sign, and a process that can reach the session
// bus can ask the keyring for items. They are here because a vault that
// cannot reach its agent or its keyring cannot answer, and dropping them
// would break the backends this package exists to support. Anything that
// needs a stricter environment than this should be given its own
// dedicated agent socket rather than the operator's.
//
// Credentials reach a provider by being declared: ProviderConfig.Env,
// built by FromConfig from the provider's own env and secret_env blocks.
// Declaring one IS the authorisation, so those bypass the allowlist
// entirely. What is gone is the third route — a variable neither
// declared nor needed, arriving because it happened to be in the
// parent's environment.
var baseEnvAllowlist = []string{
	// Identity and filesystem. `pass` and `gpg` resolve their store
	// under HOME; every CLI resolves its own binary through PATH.
	"HOME",
	"LOGNAME",
	"PATH",
	"TMPDIR",
	"USER",

	// Locale. A CLI that cannot determine an encoding may emit
	// replacement characters into the secret itself.
	"LANG",
	"LC_ALL",
	"LC_CTYPE",
	"TZ",

	// XDG base directories. `op` keeps its config under
	// XDG_CONFIG_HOME, and gpg-agent's socket lives under
	// XDG_RUNTIME_DIR.
	"XDG_CACHE_HOME",
	"XDG_CONFIG_HOME",
	"XDG_DATA_HOME",
	"XDG_RUNTIME_DIR",

	// GnuPG. GNUPGHOME was named in the original justification for
	// inheriting the environment; GPG_TTY is how gpg finds a terminal
	// for a pinentry prompt.
	"GNUPGHOME",
	"GPG_TTY",

	// Agent and desktop sockets. SSH_AUTH_SOCK for an agent-backed
	// store, DBUS_SESSION_BUS_ADDRESS for a Secret Service backend, and
	// the display variables for a graphical pinentry.
	//
	// These are capability handles rather than secrets, and the
	// distinction is thinner than it sounds: the value is a socket path,
	// but reaching the socket is reaching the agent. See the package
	// comment above.
	"DBUS_SESSION_BUS_ADDRESS",
	"DISPLAY",
	"SSH_AUTH_SOCK",
	"WAYLAND_DISPLAY",
}

// vendorEnv is one driver's contribution to the allowlist.
//
// prefixes exist for one reason: `op signin` exports a per-account
// session variable whose name is not fixed (OP_SESSION_<shorthand> in
// 1Password v1), so an exact-match list cannot cover the workflow that
// opAuthHint tells operators to use.
//
// Prefixes are available to DRIVERS ONLY, never to env_passthrough. A
// driver's list is code, reviewed once; env_passthrough is operator
// input, and a glob there would let `OP_*` or `*` reinstate wholesale
// inheritance in a setting that reads as though it is being careful.
type vendorEnv struct {
	names    []string
	prefixes []string
}

// bitwardenEnv and onePasswordEnv are the variables each vendor CLI
// authenticates with.
//
// They are here rather than left to the operator because the drivers'
// own error hints name them: bwLockedHint says to export BW_SESSION, and
// opAuthHint says to run `op signin` or set OP_SERVICE_ACCOUNT_TOKEN. An
// allowlist that broke the fix its own error message recommends would be
// a worse bug than the one it closes.
//
// Both sign-in routes have to work, not just the non-interactive one.
// `op signin` is the interactive route and it exports a session variable
// rather than a token, which is why onePasswordEnv carries a prefix.
var (
	bitwardenEnv = vendorEnv{
		names: []string{
			"BITWARDENCLI_APPDATA_DIR",
			"BW_CLIENTID",
			"BW_CLIENTSECRET",
			"BW_SESSION",
		},
	}
	onePasswordEnv = vendorEnv{
		names: []string{
			"OP_ACCOUNT",
			"OP_CONFIG_DIR",
			"OP_CONNECT_HOST",
			"OP_CONNECT_TOKEN",
			"OP_SERVICE_ACCOUNT_TOKEN",
			// The bare form, for setups that export it directly.
			"OP_SESSION",
		},
		// `eval $(op signin <shorthand>)` exports OP_SESSION_<shorthand>.
		prefixes: []string{"OP_SESSION_"},
	}
)

// allowedEnv is the subprocess environment: the allowlisted slice of the
// parent's, plus everything the provider declared.
//
// Declared values are appended last so they win on a duplicate name.
// exec.Cmd takes the last occurrence, and the only sound precedence is
// that an explicitly configured value beats an inherited one — the other
// way round, a stale variable in the node's environment would silently
// shadow the config.
func allowedEnv(declared map[string]string, extra vendorEnv) []string {
	allow := make(map[string]struct{}, len(baseEnvAllowlist)+len(extra.names))
	for _, k := range baseEnvAllowlist {
		allow[k] = struct{}{}
	}
	for _, k := range extra.names {
		if k = strings.TrimSpace(k); k != "" {
			allow[k] = struct{}{}
		}
	}

	parent := os.Environ()
	out := make([]string, 0, len(allow)+len(declared))
	for _, kv := range parent {
		// SplitN, not Split: a value may legitimately contain "=".
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		if _, ok := allow[parts[0]]; ok {
			out = append(out, kv)
			continue
		}
		for _, p := range extra.prefixes {
			if p != "" && strings.HasPrefix(parts[0], p) {
				out = append(out, kv)
				break
			}
		}
	}

	keys := make([]string, 0, len(declared))
	for k := range declared {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+declared[k])
	}
	return out
}

// parseEnvPassthrough reads the env_passthrough option: a
// comma-separated list of variable names to add to the allowlist.
//
// Names, never patterns. A glob would let `*` or `SECRET_*` reinstate
// the behaviour this file removes, in a config setting that reads as
// though it is being careful. Driver-contributed prefixes exist
// (vendorEnv.prefixes) but are not reachable from config.
//
// A name containing `*` is REFUSED at boot rather than matched
// literally. Matching it literally would be safe but silent, and a
// setting that parses and then does nothing is the failure mode this
// package's option validation exists to prevent.
func parseEnvPassthrough(label, v string) ([]string, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	fields := strings.Split(v, ",")
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if strings.ContainsAny(f, "*?[") {
			return nil, fmt.Errorf(
				"secrets: provider %q: env_passthrough %q looks like a pattern; "+
					"it takes exact variable names, comma separated", label, f)
		}
		out = append(out, f)
	}
	return out, nil
}
