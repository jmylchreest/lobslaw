package config

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// LoadDotenv populates the process environment from a .env file if
// one exists at path (or via the same discovery chain as config.toml
// when path is empty). Existing env vars WIN — a value in .env
// doesn't override one already set externally. This matches the
// Unix convention that the outer environment is authoritative.
//
// Supported syntax (a practical subset of the de-facto .env format):
//
//	KEY=value          # plain
//	KEY="with spaces"  # double-quoted; escapes: \n \t \\ \"
//	KEY='literal'      # single-quoted; no escaping
//	export KEY=value   # leading `export ` tolerated (shell compat)
//	# comment          # whole-line comment
//	KEY=value # inline # tolerated ONLY after unquoted values
//
// Unsupported: shell expansion (`${OTHER_VAR}`), command
// substitution, variable references. If you need those, set the
// variable in your shell before running lobslaw — .env here is
// for simple secret / config bootstrapping, not a shell replacement.
//
// Missing file is NOT an error — the function returns nil so
// callers can invoke it unconditionally at boot. Syntax errors
// ARE returned so typos don't silently hide variables.
func LoadDotenv(path string) error {
	if path == "" {
		p, err := findDotenvPath()
		if err != nil {
			return err
		}
		if p == "" {
			return nil
		}
		path = p
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open .env %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	pairs, err := parseDotenv(f)
	if err != nil {
		return fmt.Errorf("parse .env %q: %w", path, err)
	}
	for _, kv := range pairs {
		if _, set := os.LookupEnv(kv.key); set {
			continue
		}
		if err := os.Setenv(kv.key, kv.value); err != nil {
			return fmt.Errorf("set %q: %w", kv.key, err)
		}
	}
	return nil
}

// findDotenvPath mirrors findConfigPath but for .env. Priority:
// $LOBSLAW_ENV > ./.env > $XDG_CONFIG_HOME/lobslaw/.env >
// $HOME/.config/lobslaw/.env. Returns empty string (no error) when
// no file exists in the chain — callers treat that as "no .env to
// load".
func findDotenvPath() (string, error) {
	if p := os.Getenv("LOBSLAW_ENV"); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("LOBSLAW_ENV=%q: %w", p, err)
		}
		return p, nil
	}
	candidates := []string{"./.env"}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "lobslaw", ".env"))
	} else if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "lobslaw", ".env"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("stat %q: %w", c, err)
		}
	}
	return "", nil
}

// dotenvPair is a parsed key/value. Exported only for test use via
// parseDotenvPairs; actual callers go through LoadDotenv which
// applies the os.Setenv step.
type dotenvPair struct {
	key   string
	value string
}

// parseDotenv reads the full io.Reader and returns all key/value
// pairs in file order. Syntax errors include the offending line
// number so operators can locate the problem quickly.
func parseDotenv(r io.Reader) ([]dotenvPair, error) {
	scanner := bufio.NewScanner(r)
	// Allow lines up to 1MB — some API keys / JSON blobs get long.
	scanner.Buffer(make([]byte, 64*1024), 1<<20)

	var out []dotenvPair
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		kv, err := parseDotenvLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if kv == nil {
			continue
		}
		out = append(out, *kv)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// parseDotenvLine returns (nil, nil) for blank and comment-only
// lines; (pair, nil) for an assignment; or an error with a clear
// message. The parser is hand-rolled rather than regex-based so
// quoting and edge cases are explicit.
func parseDotenvLine(line string) (*dotenvPair, error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return nil, nil
	}
	// Tolerate leading `export ` for shell compatibility.
	trimmed = strings.TrimPrefix(trimmed, "export ")
	trimmed = strings.TrimLeft(trimmed, " \t")

	eqIdx := strings.Index(trimmed, "=")
	if eqIdx <= 0 {
		return nil, errors.New(`missing "=" or empty key`)
	}
	key := strings.TrimRight(trimmed[:eqIdx], " \t")
	if !validKey(key) {
		return nil, fmt.Errorf("invalid key %q — keys must match [A-Za-z_][A-Za-z0-9_]*", key)
	}
	rhs := strings.TrimLeft(trimmed[eqIdx+1:], " \t")

	value, err := parseValue(rhs)
	if err != nil {
		return nil, err
	}
	return &dotenvPair{key: key, value: value}, nil
}

// validKey returns true when s is a legal POSIX-ish env var name.
// Reject anything that wouldn't round-trip through a bash export.
func validKey(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		first := i == 0
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c == '_':
		case !first && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

// parseValue handles the three RHS shapes: double-quoted (with
// escape sequences), single-quoted (literal), or unquoted (strips
// trailing comment + whitespace).
func parseValue(rhs string) (string, error) {
	if rhs == "" {
		return "", nil
	}
	switch rhs[0] {
	case '"':
		return parseDoubleQuoted(rhs)
	case '\'':
		return parseSingleQuoted(rhs)
	default:
		return parseUnquoted(rhs), nil
	}
}

// parseDoubleQuoted finds the matching closing quote and processes
// \n / \t / \\ / \" escapes inside. Content past the closing quote
// is allowed only if it's a comment (starts with # after optional
// whitespace).
func parseDoubleQuoted(s string) (string, error) {
	var out strings.Builder
	i := 1
	for i < len(s) {
		c := s[i]
		switch {
		case c == '"':
			if rest := strings.TrimSpace(s[i+1:]); rest != "" && !strings.HasPrefix(rest, "#") {
				return "", fmt.Errorf("unexpected content after closing quote: %q", rest)
			}
			return out.String(), nil
		case c == '\\' && i+1 < len(s):
			next := s[i+1]
			switch next {
			case 'n':
				out.WriteByte('\n')
			case 't':
				out.WriteByte('\t')
			case 'r':
				out.WriteByte('\r')
			case '\\':
				out.WriteByte('\\')
			case '"':
				out.WriteByte('"')
			default:
				out.WriteByte('\\')
				out.WriteByte(next)
			}
			i += 2
			continue
		default:
			out.WriteByte(c)
		}
		i++
	}
	return "", errors.New("unterminated double-quoted string")
}

// parseSingleQuoted finds the matching closing quote. Single quotes
// are LITERAL — no escape sequences. Content past the closing quote
// must be comment-or-nothing (same rule as double-quoted).
func parseSingleQuoted(s string) (string, error) {
	end := strings.Index(s[1:], "'")
	if end < 0 {
		return "", errors.New("unterminated single-quoted string")
	}
	end++ // adjust for the [1:] offset
	rest := strings.TrimSpace(s[end+1:])
	if rest != "" && !strings.HasPrefix(rest, "#") {
		return "", fmt.Errorf("unexpected content after closing quote: %q", rest)
	}
	return s[1:end], nil
}

// parseUnquoted strips a trailing inline comment ("# ...") and any
// trailing whitespace. Leading whitespace was already trimmed by
// the caller. Quotes inside unquoted values are literal characters.
func parseUnquoted(s string) string {
	if i := strings.Index(s, " #"); i >= 0 {
		s = s[:i]
	} else if i := strings.Index(s, "\t#"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimRight(s, " \t")
}
