package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
)

// Resolver is the subset of net.Resolver behaviour ExpandSeeds uses.
// Tests can supply a fake; production uses net.DefaultResolver.
type Resolver interface {
	LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// DefaultResolver wraps net.DefaultResolver. A named alias so callers
// can write discovery.DefaultResolver without importing net just for
// this.
var DefaultResolver Resolver = net.DefaultResolver

// ExpandSeeds takes a raw seed list and expands `srv:` and `dns:`
// prefixed entries into concrete host:port addresses via the resolver.
//
// Supported forms:
//
//	host:port                            — passed through unchanged
//	srv:<full-srv-name>                  — LookupSRV returns targets; emits host:port per target
//	dns:<host>:<port>                    — LookupHost returns A/AAAA; emits <ip>:<port> per result
//
// Unresolvable entries are logged at Warn level and skipped — callers
// already tolerate partial success (a single reachable seed bootstraps).
// Duplicates are de-duped; the output is sorted for deterministic
// dial order (useful for tests, harmless in production).
func ExpandSeeds(ctx context.Context, seeds []string, resolver Resolver, logger *slog.Logger) []string {
	if resolver == nil {
		resolver = DefaultResolver
	}
	if logger == nil {
		logger = slog.Default()
	}

	seen := make(map[string]struct{}, len(seeds))
	add := func(addr string) {
		if addr == "" {
			return
		}
		if _, exists := seen[addr]; exists {
			return
		}
		seen[addr] = struct{}{}
	}

	for _, seed := range seeds {
		seed = strings.TrimSpace(seed)
		switch {
		case strings.HasPrefix(seed, "srv:"):
			name := strings.TrimPrefix(seed, "srv:")
			// net.LookupSRV returns (cname, srvs, err); we only need
			// the SRV target list.
			_, targets, err := resolver.LookupSRV(ctx, "", "", name)
			if err != nil {
				logger.Warn("srv seed resolution failed", "seed", seed, "err", err)
				continue
			}
			for _, t := range targets {
				host := strings.TrimSuffix(t.Target, ".")
				add(net.JoinHostPort(host, fmt.Sprintf("%d", t.Port)))
			}

		case strings.HasPrefix(seed, "dns:"):
			rest := strings.TrimPrefix(seed, "dns:")
			host, port, err := net.SplitHostPort(rest)
			if err != nil {
				logger.Warn("dns seed must be dns:<host>:<port>", "seed", seed, "err", err)
				continue
			}
			ips, err := resolver.LookupHost(ctx, host)
			if err != nil {
				logger.Warn("dns seed resolution failed", "seed", seed, "err", err)
				continue
			}
			for _, ip := range ips {
				add(net.JoinHostPort(ip, port))
			}

		case seed == "":
			// skip blank entries — operators may have commented-out lines

		default:
			// plain host:port; accept as-is
			add(seed)
		}
	}

	out := make([]string, 0, len(seen))
	for addr := range seen {
		out = append(out, addr)
	}
	sort.Strings(out)
	return out
}
