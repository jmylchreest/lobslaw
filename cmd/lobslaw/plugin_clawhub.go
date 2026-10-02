package main

import "errors"

// Compatibility entry point: same flags and implementation as skills install.
// Local root/yes options must not silently become activation authority.
func pluginInstallClawhub(source, root string, yes bool, opts *shareInstallOptions) error {
	if root != "" {
		return errors.New("ClawHub --root installs are retired; use --owner and a running cluster for reviewed staging")
	}
	if yes {
		return errors.New("ClawHub --yes is retired; preview, then use --apply --expected-plan; activation remains separate")
	}
	diagnosticf("plugin install clawhub: now uses skills install; this previews/stages only. Activate separately with skills activate-install.\n")
	return opts.run(source)
}
