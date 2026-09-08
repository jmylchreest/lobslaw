//go:build linux

package secrets

import (
	"fmt"

	dbus "github.com/godbus/dbus/v5"
	"github.com/zalando/go-keyring"
)

// newSecretServiceProvider dials the session bus, then returns a provider
// over the keyring-backed lookup. At boot because a container has no
// session bus and nothing in the config hints at it, and godbus hides
// that: dbus.SessionBus() falls back to `dbus-launch`, which where
// installed succeeds and starts a fresh private bus with no keyring on
// it, so a missing bind mount looks exactly like a wrong item name.
// SessionBusPrivateNoAutoStartup resolves the address the same way and
// refuses the autostart; the dial catches a socket nobody is serving.
func newSecretServiceProvider(cfg ProviderConfig) (Provider, error) {
	conn, err := dbus.SessionBusPrivateNoAutoStartup()
	if err != nil {
		return nil, fmt.Errorf("secrets: provider %q: driver = %q: %w; %s",
			cfg.Label, DriverSecretService, err, secretServiceNoBusHint)
	}
	// Closed straight away: this was a reachability check, nothing more.
	// Fetches use go-keyring's own connection.
	_ = conn.Close()

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultFetchTimeout
	}
	return &secretServiceProvider{label: cfg.Label, get: keyring.Get, timeout: timeout}, nil
}
