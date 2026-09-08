package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	dbus "github.com/godbus/dbus/v5"
	"github.com/zalando/go-keyring"
)

// A compiled driver over org.freedesktop.secrets, the D-Bus interface
// every Linux desktop keyring already implements. Not per-vendor like
// vendor_cli.go's drivers: gnome-keyring, KWallet, KeePassXC and rosec
// all answer on the same interface, so one driver reaches a whole class
// of backend, and it wants a socket mounted rather than a CLI on PATH. It
// spawns nothing, so env.go's allowlist has no bearing on it, even though
// the bus address is listed there for the exec-family drivers.
//
// go-keyring looks an item up by exactly {service, username} and no other
// attribute, which is why the reference shape is a two-part path; and its
// GetLoginCollection() is hardcoded to the login collection, so a
// `collection` option would parse and change nothing, which is the
// failure unknownOptions exists to prevent. Past those limits, drop to
// godbus/dbus/v5 and drive org.freedesktop.Secret.Service directly.

// The fixes this driver exists to name. Long because the useful part of
// each is a command or a pair of container flags, not a diagnosis.
const (
	secretServiceLockedHint = "the collection is locked; unlock it in the keyring itself " +
		"(`rosec unlock`, or answer the desktop's own prompt). A headless or containerised " +
		"node has no prompt to answer, so the collection has to be unlocked before lobslaw starts"

	secretServiceNoBusHint = "no D-Bus session bus is reachable, so DBUS_SESSION_BUS_ADDRESS " +
		"is unset or points at nothing. A container has neither by default: bind-mount the " +
		"socket and name it, with `--volume $XDG_RUNTIME_DIR/bus:/run/user/1000/bus " +
		"--env DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus`. That hands the " +
		"container the whole Secret Service rather than one item, so read the caveat in " +
		"docs/configuration/secrets first"

	secretServiceNoImplHint = "the session bus answered but nothing on it is serving " +
		"org.freedesktop.secrets; start a keyring in the same session " +
		"(gnome-keyring-daemon, kwalletd, keepassxc, rosec)"
)

// D-Bus error names this driver recognises. Names rather than message
// text: the name is the stable half of the wire protocol.
const (
	dbusErrServiceUnknown = "org.freedesktop.DBus.Error.ServiceUnknown"
	secretErrIsLocked     = "org.freedesktop.Secret.Error.IsLocked"
)

// secretGetter is the one operation this driver needs from a Secret
// Service client. A function rather than the library call inline, so the
// split, the timeout and the error translation stay testable off Linux.
type secretGetter func(service, user string) (string, error)

// SecretServiceFactory builds a provider over org.freedesktop.secrets.
func SecretServiceFactory(cfg ProviderConfig) (Provider, error) {
	// No options, deliberately: trim_whitespace and env_passthrough both
	// configure a subprocess, and this driver spawns none.
	if bad := unknownOptions(cfg.Options); len(bad) > 0 {
		return nil, fmt.Errorf("secrets: provider %q: unknown option(s) %v; "+
			"driver = %q takes no options", cfg.Label, bad, DriverSecretService)
	}
	// Refused rather than ignored: with no process to run, both would parse
	// cleanly and configure nothing, and a silently accepted `command`
	// would let an operator believe a wrapper script was being run.
	if len(cfg.Command) > 0 {
		return nil, fmt.Errorf("secrets: provider %q: driver = %q talks D-Bus in process "+
			"and runs no command, so `command` would configure nothing; use "+
			"driver = %q to run a CLI", cfg.Label, DriverSecretService, DriverExec)
	}
	if len(cfg.Env) > 0 {
		return nil, fmt.Errorf("secrets: provider %q: driver = %q spawns no process, so "+
			"`env` and `secret_env` have nowhere to go; DBUS_SESSION_BUS_ADDRESS is read "+
			"from lobslaw's own environment", cfg.Label, DriverSecretService)
	}
	// The platform half of the driver, in secretservice_linux.go and
	// secretservice_other.go. Returned rather than assigned and checked,
	// because on a non-Linux build it never succeeds and `if err != nil`
	// there is a comparison staticcheck can prove is always true (SA4023).
	return newSecretServiceProvider(cfg)
}

type secretServiceProvider struct {
	label   string
	get     secretGetter
	timeout time.Duration
}

func (p *secretServiceProvider) Fetch(ctx context.Context, path string) (string, error) {
	service, user, err := splitItemPath(p.label, path)
	if err != nil {
		return "", err
	}

	// keyring.Get takes no context, so the lookup gets its own goroutine
	// and this one stops waiting at the timeout. A locked collection raises
	// a prompt that go-keyring blocks on with no deadline, and a headless
	// node has nobody to answer it, so an unbounded fetch would hang a
	// boot-time resolve. The price is a goroutine parked on a D-Bus signal,
	// the same trade exec.go makes with WaitDelay: better than a node that
	// never finishes starting.
	type result struct {
		value string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		v, err := p.get(service, user)
		done <- result{value: v, err: err}
	}()

	timer := time.NewTimer(p.timeout)
	defer timer.Stop()

	select {
	case r := <-done:
		if r.err != nil {
			return "", p.fetchError(r.err, service, user)
		}
		if r.value == "" {
			return "", fmt.Errorf("secrets: provider %q: the item for service %q, user %q "+
				"exists but holds nothing", p.label, service, user)
		}
		return r.value, nil
	case <-timer.C:
		return "", fmt.Errorf("secrets: provider %q: org.freedesktop.secrets did not answer "+
			"within %s for service %q, user %q; %s",
			p.label, p.timeout, service, user, secretServiceLockedHint)
	case <-ctx.Done():
		return "", fmt.Errorf("secrets: provider %q: %w", p.label, ctx.Err())
	}
}

// fetchError wraps a client failure with the provider, and with the fix
// where the failure is a recognised one. The hint is appended, never
// substituted: guessing wrong must not hide what the library said.
func (p *secretServiceProvider) fetchError(err error, service, user string) error {
	wrapped := fmt.Errorf("secrets: provider %q: org.freedesktop.secrets: %w", p.label, err)
	if hint := secretServiceHintFor(err, service, user); hint != "" {
		return fmt.Errorf("%w; %s", wrapped, hint)
	}
	return wrapped
}

// secretServiceHintFor names the fix for a failure it recognises, and
// returns "" for one it does not. Ordered best-typed first, which is also
// least fragile first: a sentinel for keyring.ErrNotFound; errors.As for a
// dbus.Error, whose Error() returns the message body and not the name, so
// the name is unreachable by substring (a value, not a pointer, which is
// godbus serving an export rather than calling one); then substrings.
func secretServiceHintFor(err error, service, user string) string {
	if errors.Is(err, keyring.ErrNotFound) {
		// The locked clause is not padding: a collection that fails to
		// unlock returns no search results rather than an error.
		return fmt.Sprintf("no item matched service %q, user %q, which is what the "+
			"reference path became when split on its last slash; check that split first, "+
			"then that the item carries both attributes (`secret-tool search service %s`). "+
			"A locked collection can also present as a miss", service, user, service)
	}

	var dbusErr dbus.Error
	if errors.As(err, &dbusErr) {
		switch dbusErr.Name {
		case dbusErrServiceUnknown:
			return secretServiceNoImplHint
		case secretErrIsLocked:
			return secretServiceLockedHint
		}
	}

	low := strings.ToLower(err.Error())
	for _, m := range []string{
		// go-keyring's wording when Unlock returns without the collection it
		// asked for, which is what a dismissed prompt produces.
		"failed to unlock",
		"prompt dismissed",
		"dismissed",
		"is locked",
	} {
		if strings.Contains(low, m) {
			return secretServiceLockedHint
		}
	}
	for _, m := range []string{
		"couldn't determine address of session bus",
		// An address with nothing listening: net.Dial's *net.OpError leads
		// with the operation and network.
		"dial unix",
		"connection refused",
		// The autostart fallback, reachable only if the boot-time check in
		// secretservice_linux.go was somehow bypassed.
		"dbus-launch",
	} {
		if strings.Contains(low, m) {
			return secretServiceNoBusHint
		}
	}
	return ""
}

// splitItemPath turns a reference path into the (service, user) pair
// go-keyring looks an item up by, splitting on the LAST slash because the
// user is the leaf: "lobslaw/prod/openrouter" is the openrouter key under
// service "lobslaw/prod", where the first slash would ask for a user
// called "prod/openrouter". A bare name takes the label as the service.
func splitItemPath(label, path string) (service, user string, err error) {
	path = strings.TrimSpace(path)
	if i := strings.LastIndex(path, "/"); i >= 0 {
		service, user = path[:i], path[i+1:]
	} else {
		service, user = label, path
	}
	// An empty half is a leading or trailing slash, which left alone
	// searches for an item whose username is the empty string.
	if service == "" || user == "" {
		return "", "", fmt.Errorf("secrets: provider %q: path %q does not name an item; "+
			"the shape is service/user split on the last slash, as in %s:lobslaw/openrouter, "+
			"or a bare name that takes %q as the service",
			label, path, label, label)
	}
	return service, user, nil
}
