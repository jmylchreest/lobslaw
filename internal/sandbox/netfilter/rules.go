package netfilter

import (
	"fmt"
	"net"
	"sort"
	"strings"
)

// TableName is the nftables table the rules live under. Single
// fixed name so an Apply re-run cleanly replaces a prior install
// (delete-then-create idempotency).
const TableName = "lobslaw_egress"

// RuleSet is the structured input to GenerateRules. Operators
// describe what the subprocess should be allowed to reach; the
// generator produces the nft script that enforces it.
type RuleSet struct {
	// AllowLoopback admits traffic on the lo interface — required
	// when smokescreen reaches the subprocess via UDS bind-mount
	// (the proxy connection itself is over loopback).
	AllowLoopback bool

	// AllowCIDRs is the operator-declared CIDR allowlist (matches
	// sandbox.Policy.NetworkAllowCIDR). IPv4 + IPv6 supported;
	// invalid entries error in GenerateRules.
	AllowCIDRs []string

	// AllowDNS admits UDP+TCP port 53 to any destination. Most skills
	// need DNS to resolve hostnames they then connect to via the
	// proxy; without this, even loopback-bound HTTPS_PROXY URLs
	// can't resolve "localhost" via nss.
	AllowDNS bool
}

// GenerateRules produces the nft script that establishes a
// "drop-by-default, allow-by-rule" output filter on a fresh netns.
// Output is the script body to feed to `nft -f -`. Returned text
// is deterministic so tests can compare directly.
//
// The rules add CIDR + interface allowances under a final "drop"
// fall-through. A subprocess with no rules at all gets the same
// effective behaviour as having only the default-drop policy on
// the output chain — but we still install the table so cleanup
// (TeardownScript) can find it by name.
func GenerateRules(rs RuleSet) (string, error) {
	if err := validate(rs); err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "add table inet %s\n", TableName)
	fmt.Fprintf(&b, "add chain inet %s output { type filter hook output priority 0; policy drop; }\n", TableName)
	if rs.AllowLoopback {
		fmt.Fprintf(&b, "add rule inet %s output meta oif lo accept\n", TableName)
	}
	if rs.AllowDNS {
		fmt.Fprintf(&b, "add rule inet %s output udp dport 53 accept\n", TableName)
		fmt.Fprintf(&b, "add rule inet %s output tcp dport 53 accept\n", TableName)
	}

	v4, v6 := splitCIDRs(rs.AllowCIDRs)
	for _, c := range v4 {
		fmt.Fprintf(&b, "add rule inet %s output ip daddr %s accept\n", TableName, c)
	}
	for _, c := range v6 {
		fmt.Fprintf(&b, "add rule inet %s output ip6 daddr %s accept\n", TableName, c)
	}
	return b.String(), nil
}

// TeardownScript removes the lobslaw_egress table. Used when the
// subprocess exits cleanly OR when re-applying rules across a
// re-invocation (delete-then-create). A non-existent table is
// not an error.
func TeardownScript() string {
	return fmt.Sprintf("delete table inet %s\n", TableName)
}

// validate fails on malformed CIDR entries before we hand the
// rules to nft. nft's error messages on bad input are cryptic
// and surface only at apply-time; catching it earlier gives the
// operator a clear "rule i has bad CIDR" diagnostic.
func validate(rs RuleSet) error {
	for i, c := range rs.AllowCIDRs {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(c)); err != nil {
			return fmt.Errorf("netfilter: AllowCIDRs[%d] %q: %w", i, c, err)
		}
	}
	return nil
}

func splitCIDRs(cidrs []string) (v4, v6 []string) {
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		_, ipNet, err := net.ParseCIDR(c)
		if err != nil {
			continue
		}
		if ipNet.IP.To4() != nil {
			v4 = append(v4, c)
		} else {
			v6 = append(v6, c)
		}
	}
	sort.Strings(v4)
	sort.Strings(v6)
	return v4, v6
}
