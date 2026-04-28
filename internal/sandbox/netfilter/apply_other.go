//go:build !linux

package netfilter

import (
	"context"
	"errors"
)

// errUnsupported surfaces when a non-Linux build attempts to apply
// netfilter rules. Non-Linux deployments rely on Phase E.4
// HTTPS_PROXY env-level enforcement; the kernel-level belt-and-
// braces step requires Linux nftables.
var errUnsupported = errors.New("netfilter: nftables enforcement is Linux-only; non-Linux falls back to HTTPS_PROXY-only enforcement")

func Apply(_ context.Context, _ RuleSet) error { return errUnsupported }

func Teardown(_ context.Context) error { return errUnsupported }
