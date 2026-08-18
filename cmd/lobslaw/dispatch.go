package main

import (
	"flag"
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
		"--context":   "LOBSLAW_CONTEXT",
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
	"--context":    true,
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

// parseFlagsAndPositionals parses a flag set whose positional
// arguments may appear BEFORE the flags.
//
// Go's flag package stops at the first non-flag argument, so
// `cluster export-operator alice --out ./alice` leaves alice AND every
// flag in Args() — and the command then rejects its own documented
// usage. The loop re-parses the tail after each positional, which is
// the standard idiom for this.
//
// Returns the positionals in order.
func parseFlagsAndPositionals(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}
