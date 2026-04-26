package main

import (
	"os"
	"strings"
)

// hoistGlobalFlagsToEnv pulls a known set of global flags out of
// argv and stashes their values in $LOBSLAW_* env vars so
// subcommands (which parse their own flag set) can default to them.
// Idempotent — only sets env vars that aren't already populated, so
// existing $LOBSLAW_* settings still win.
func hoistGlobalFlagsToEnv(args []string) {
	mapping := map[string]string{
		"--config":    "LOBSLAW_CONFIG",
		"--env":       "LOBSLAW_ENV",
		"--log-level": "LOBSLAW_LOG_LEVEL",
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return
		}
		if !strings.HasPrefix(a, "--") {
			continue
		}
		name, val, hasEq := strings.Cut(a, "=")
		envKey, ok := mapping[name]
		if !ok {
			continue
		}
		if !hasEq {
			if i+1 >= len(args) {
				continue
			}
			val = args[i+1]
			i++
		}
		if _, set := os.LookupEnv(envKey); set {
			continue
		}
		_ = os.Setenv(envKey, val)
	}
}

// globalValueFlags lists the long-form global flags that take a
// separate value token. Used by findSubcmd so it can skip past them
// when an operator writes `lobslaw --config foo cluster sign-node`.
// Keep in sync with parseFlags() in main.go.
var globalValueFlags = map[string]bool{
	"--config":     true,
	"--env":        true,
	"--log-level":  true,
	"--log-format": true,
	"--policy-dir": true,
}

// findSubcmd walks args and returns the index of name when it
// appears as the first non-flag positional, or -1 if it doesn't.
// Recognises `--flag=value` (single token) and `--flag value`
// (two tokens) for the global value-flags. A bare `--` ends flag
// parsing — anything after it is considered positional.
func findSubcmd(args []string, name string) int {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) && args[i+1] == name {
				return i + 1
			}
			return -1
		}
		if strings.HasPrefix(a, "-") {
			if strings.HasPrefix(a, "--") && strings.Contains(a, "=") {
				continue
			}
			if globalValueFlags[a] {
				i++
			}
			continue
		}
		if a == name {
			return i
		}
		return -1
	}
	return -1
}
