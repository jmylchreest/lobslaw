//go:build linux

package netfilter

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// nftBinary is the path to the userspace nftables tool. Resolved
// via PATH at apply time; operators can override by symlinking a
// statically-linked nft into /usr/local/bin if the host distro
// doesn't ship one. Static binary used because Apply runs after
// the subprocess has unshare()d into its own mount namespace where
// /usr/sbin/nft from the host might not be visible.
const nftBinary = "nft"

// applyTimeout caps how long nft can take to install rules. A
// healthy nft completes in <100ms; anything longer indicates a
// hung netlink socket or kernel bug. Killing the process and
// surfacing a timeout is preferable to wedging the skill launch.
const applyTimeout = 5 * time.Second

// Apply installs the rules in whatever netns the calling goroutine
// is currently attached to. The caller is responsible for entering
// the target netns first (typically via setns + LockOSThread). On
// most modern kernels CAP_NET_ADMIN within the user namespace
// suffices — no real-root privileges required.
//
// Returns an error if `nft` is missing from the netns's mount
// namespace, exits non-zero, or doesn't complete within
// applyTimeout. The caller decides whether to fail the launch or
// fall back to HTTPS_PROXY-only enforcement.
func Apply(ctx context.Context, rs RuleSet) error {
	script, err := GenerateRules(rs)
	if err != nil {
		return err
	}
	return runNft(ctx, script)
}

// Teardown removes the rules. Safe to call even if Apply was never
// invoked — `delete table` on a missing table is a soft error nft
// reports via stderr but exit code zero on most versions; we treat
// stderr-only complaints as success.
func Teardown(ctx context.Context) error {
	return runNft(ctx, TeardownScript())
}

func runNft(ctx context.Context, script string) error {
	if strings.TrimSpace(script) == "" {
		return errors.New("netfilter: empty script")
	}
	if _, err := exec.LookPath(nftBinary); err != nil {
		return fmt.Errorf("netfilter: %s not found in PATH: %w", nftBinary, err)
	}
	runCtx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, nftBinary, "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("netfilter: nft -f -: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
