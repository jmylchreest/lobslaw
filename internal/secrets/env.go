package secrets

import (
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
// or an agent is reached through. Nothing in it is a credential.
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
	// the display variables for a graphical pinentry. Addresses of
	// sockets, not secrets — though a bus address does reach the user's
	// whole keyring, which is the caveat #196 raises about containers.
	"DBUS_SESSION_BUS_ADDRESS",
	"DISPLAY",
	"SSH_AUTH_SOCK",
	"WAYLAND_DISPLAY",
}

// bitwardenEnvAllowlist and onePasswordEnvAllowlist are the variables
// each vendor CLI authenticates with.
//
// They are here rather than left to the operator because the drivers'
// own error hints tell operators to export BW_SESSION and
// OP_SERVICE_ACCOUNT_TOKEN. An allowlist that broke the fix its own
// error message recommends would be a worse bug than the one it closes.
var (
	bitwardenEnvAllowlist = []string{
		"BITWARDENCLI_APPDATA_DIR",
		"BW_CLIENTID",
		"BW_CLIENTSECRET",
		"BW_SESSION",
	}
	onePasswordEnvAllowlist = []string{
		"OP_ACCOUNT",
		"OP_CONFIG_DIR",
		"OP_CONNECT_HOST",
		"OP_CONNECT_TOKEN",
		"OP_SERVICE_ACCOUNT_TOKEN",
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
func allowedEnv(declared map[string]string, extraAllowed []string) []string {
	allow := make(map[string]struct{}, len(baseEnvAllowlist)+len(extraAllowed))
	for _, k := range baseEnvAllowlist {
		allow[k] = struct{}{}
	}
	for _, k := range extraAllowed {
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

// parseEnvPassthrough reads the exec driver's env_passthrough option: a
// comma-separated list of variable names to add to the allowlist.
//
// Names, never patterns. A glob would let `*` or `SECRET_*` reinstate
// the behaviour this file removes, in a config setting that reads as
// though it is being careful.
func parseEnvPassthrough(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	fields := strings.Split(v, ",")
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
