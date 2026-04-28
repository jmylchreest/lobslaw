package netfilter

import (
	"strings"
	"testing"
)

func TestGenerateRulesIncludesTableAndDropChain(t *testing.T) {
	t.Parallel()
	out, err := GenerateRules(RuleSet{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "add table inet "+TableName) {
		t.Errorf("missing table create: %s", out)
	}
	if !strings.Contains(out, "policy drop") {
		t.Errorf("output chain must default-drop: %s", out)
	}
}

func TestGenerateRulesAllowLoopback(t *testing.T) {
	t.Parallel()
	out, _ := GenerateRules(RuleSet{AllowLoopback: true})
	if !strings.Contains(out, "meta oif lo accept") {
		t.Errorf("loopback rule missing: %s", out)
	}
}

func TestGenerateRulesAllowDNS(t *testing.T) {
	t.Parallel()
	out, _ := GenerateRules(RuleSet{AllowDNS: true})
	if !strings.Contains(out, "udp dport 53 accept") {
		t.Errorf("UDP DNS rule missing: %s", out)
	}
	if !strings.Contains(out, "tcp dport 53 accept") {
		t.Errorf("TCP DNS rule missing: %s", out)
	}
}

func TestGenerateRulesSeparatesIPv4FromIPv6(t *testing.T) {
	t.Parallel()
	out, err := GenerateRules(RuleSet{
		AllowCIDRs: []string{"10.0.0.0/8", "2001:db8::/32"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ip daddr 10.0.0.0/8 accept") {
		t.Errorf("IPv4 rule missing: %s", out)
	}
	if !strings.Contains(out, "ip6 daddr 2001:db8::/32 accept") {
		t.Errorf("IPv6 rule missing: %s", out)
	}
}

func TestGenerateRulesRejectsBadCIDR(t *testing.T) {
	t.Parallel()
	if _, err := GenerateRules(RuleSet{AllowCIDRs: []string{"not-a-cidr"}}); err == nil {
		t.Error("malformed CIDR should fail")
	}
}

func TestGenerateRulesIsDeterministic(t *testing.T) {
	t.Parallel()
	rs := RuleSet{
		AllowLoopback: true,
		AllowDNS:      true,
		AllowCIDRs:    []string{"10.0.0.0/8", "192.168.0.0/16", "2001:db8::/32"},
	}
	out1, _ := GenerateRules(rs)
	out2, _ := GenerateRules(rs)
	if out1 != out2 {
		t.Errorf("output should be deterministic")
	}
}

func TestTeardownScriptDeletesTable(t *testing.T) {
	t.Parallel()
	out := TeardownScript()
	if !strings.Contains(out, "delete table inet "+TableName) {
		t.Errorf("teardown should delete the table: %s", out)
	}
}
