package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Bitwarden and 1Password, as compiled drivers over their own CLIs.
//
// Both could be an exec block — `command = ["bw", "get", "password",
// "{{path}}"]` works. They get a file each for the reason the SearXNG
// search driver did: the common failure is a locked vault or an expired
// session, the CLI reports it in a sentence that means nothing on its
// own, and the fix is a specific command the operator has to be told.
// A generic "exit status 1: mac failed" is an afternoon; "run `bw
// unlock` and export BW_SESSION" is five seconds.
//
// The transport is deliberately the CLI rather than an API. Neither
// vendor's API can be reached without its own access token, which would
// itself have to be bootstrapped from env: or file: — so an API-backed
// provider moves one secret onto disk to take another one off it. The
// CLI reuses the session or service account the operator already has.

// bitwardenNotFound and friends are the substrings each CLI puts in its
// output for the failures worth translating. Matched loosely on
// purpose: the exact wording changes between versions and a missed
// match degrades to the raw stderr, which is no worse than before.
// defaultSecretField is the field read from a vendor CLI's item when
// the reference names none. Every supported vendor calls the primary
// secret "password", whatever the item actually holds.
const defaultSecretField = "password"

const (
	bwLockedHint = "run `bw unlock`, then export BW_SESSION (or set it in the provider's env block)"
	opAuthHint   = "run `op signin`, or set OP_SERVICE_ACCOUNT_TOKEN in the provider's env block"
)

var bitwardenOptionKeys = []string{"field"}

// BitwardenFactory builds a provider over the `bw` CLI.
//
// Defaults to fetching the password field, which is what a secret
// reference almost always means. options.field selects another —
// "username", "totp", or a custom field name.
func BitwardenFactory(cfg ProviderConfig) (Provider, error) {
	if bad := unknownOptions(cfg.Options, bitwardenOptionKeys...); len(bad) > 0 {
		return nil, fmt.Errorf("secrets: provider %q: unknown option(s) %v; supported: %s",
			cfg.Label, bad, strings.Join(bitwardenOptionKeys, ", "))
	}
	field := option(cfg.Options, "field")
	if field == "" {
		field = defaultSecretField
	}
	// An operator-supplied command wins, so a wrapper script that
	// unlocks first is possible without a new driver.
	if len(cfg.Command) == 0 {
		cfg.Command = []string{"bw", "get", field, pathPlaceholder}
	}
	inner := newExecProvider(cfg)
	return &vendorProvider{
		inner: inner,
		label: cfg.Label,
		hints: []failureHint{
			{match: []string{"vault is locked", "you are not logged in", "mac failed", "session key"},
				hint: bwLockedHint},
			{match: []string{"not found", "no such"},
				hint: "check the item name or id; `bw list items --search <name>` will show what is there"},
		},
	}, nil
}

var onePasswordOptionKeys = []string{"account"}

// OnePasswordFactory builds a provider over the `op` CLI.
//
// The path is 1Password's own op:// URI without the scheme, so a
// reference reads "op:vault/item/field" and the driver reassembles what
// `op read` expects. Keeping the vendor's own addressing means an
// operator can copy the reference straight out of the 1Password UI.
func OnePasswordFactory(cfg ProviderConfig) (Provider, error) {
	if bad := unknownOptions(cfg.Options, onePasswordOptionKeys...); len(bad) > 0 {
		return nil, fmt.Errorf("secrets: provider %q: unknown option(s) %v; supported: %s",
			cfg.Label, bad, strings.Join(onePasswordOptionKeys, ", "))
	}
	if len(cfg.Command) == 0 {
		cfg.Command = []string{"op", "read"}
		if acct := option(cfg.Options, "account"); acct != "" {
			cfg.Command = append(cfg.Command, "--account", acct)
		}
		cfg.Command = append(cfg.Command, "op://"+pathPlaceholder)
	}
	inner := newExecProvider(cfg)
	return &vendorProvider{
		inner: inner,
		label: cfg.Label,
		hints: []failureHint{
			{match: []string{"not signed in", "no account", "session expired", "authorization"},
				hint: opAuthHint},
			{match: []string{"isn't an item", "not found", "could not resolve"},
				hint: `the path is vault/item/field without the "op://" prefix, e.g. "op:Private/Alibaba/credential"`},
		},
	}, nil
}

type failureHint struct {
	match []string
	hint  string
}

// vendorProvider is an execProvider whose errors have been read.
type vendorProvider struct {
	inner *execProvider
	label string
	hints []failureHint
}

func (p *vendorProvider) Fetch(ctx context.Context, path string) (string, error) {
	v, err := p.inner.Fetch(ctx, path)
	if err == nil {
		return v, nil
	}
	// Matched against the WHOLE stderr, not the displayed error. The
	// identifying sentence is not reliably inside the part that
	// survives truncation — see cmdError.
	subject := err.Error()
	var ce *cmdError
	if errors.As(err, &ce) && ce.stderr != "" {
		subject = ce.stderr
	}
	low := strings.ToLower(subject)
	for _, h := range p.hints {
		for _, m := range h.match {
			if strings.Contains(low, m) {
				// The original error is kept whole. The hint is added,
				// never substituted: guessing wrong about which failure
				// this is must not hide what the CLI actually said.
				return "", fmt.Errorf("%w — %s", err, h.hint)
			}
		}
	}
	return "", err
}
