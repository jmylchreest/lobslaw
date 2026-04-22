package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// ResolveSecret resolves a secret reference to its value.
// Schemes: "env:VAR_NAME", "file:/path". Empty returns "", nil.
// Literal values (no scheme) are rejected to stop accidental
// plaintext secrets in config files.
func ResolveSecret(ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	scheme, rest, ok := strings.Cut(ref, ":")
	if !ok {
		return "", fmt.Errorf("%w: %q (expected env:VAR or file:/path)", types.ErrUnknownSecretScheme, ref)
	}
	switch scheme {
	case "env":
		v, ok := os.LookupEnv(rest)
		if !ok {
			return "", fmt.Errorf("%w: env var %q not set", types.ErrMissingSecret, rest)
		}
		return v, nil
	case "file":
		b, err := os.ReadFile(rest)
		if err != nil {
			return "", fmt.Errorf("%w: read %q: %w", types.ErrMissingSecret, rest, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	default:
		return "", fmt.Errorf("%w: %q", types.ErrUnknownSecretScheme, scheme)
	}
}
