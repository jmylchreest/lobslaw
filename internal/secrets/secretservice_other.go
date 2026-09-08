//go:build !linux

package secrets

import (
	"fmt"
	"runtime"
)

// newSecretServiceProvider refuses this platform at boot, not on the first
// fetch. It refuses rather than quietly working because go-keyring falls
// back to the macOS Keychain and to wincred: one `driver = "secretservice"`
// line would otherwise mean a different store on every node.
func newSecretServiceProvider(cfg ProviderConfig) (Provider, error) {
	return nil, fmt.Errorf("secrets: provider %q: driver = %q needs the "+
		"org.freedesktop.secrets D-Bus interface, which is Linux only, and this node is %s; "+
		"use driver = %q with a CLI that reaches this platform's own keyring "+
		"(`security find-generic-password` on macOS)",
		cfg.Label, DriverSecretService, runtime.GOOS, DriverExec)
}
