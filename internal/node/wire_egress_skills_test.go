package node

import (
	"log/slog"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/internal/skills"
)

func skillWithNetwork(name string, hosts []string) *skills.Skill {
	return &skills.Skill{Manifest: skills.Manifest{Name: name, Network: hosts}}
}

// SkillNetworks was assigned an empty map at boot and on every refresh
// and populated nowhere, so the builder's skill/<name> loop never ran a
// single iteration and every skill fell through to DefaultAllowedHosts.
// A manifest's network: array constrained nothing.
func TestSkillNetworksArePopulatedFromTheRegistry(t *testing.T) {
	reg := skills.NewRegistry(slog.New(slog.DiscardHandler))
	reg.Put(skillWithNetwork("weather", []string{"wttr.in", "api.open-meteo.com"}))
	reg.Put(skillWithNetwork("unconfined", nil))

	n := &Node{skillRegistry: reg, log: slog.New(slog.DiscardHandler)}
	got := skillNetworks(n)

	if hosts := got["weather"]; len(hosts) != 2 || hosts[0] != "wttr.in" {
		t.Errorf("weather hosts = %v, want the two declared", hosts)
	}
	// A skill that declared nothing must NOT appear: the builder reads
	// an empty host list as an explicit deny, and every skill written
	// before this was populated omits the field.
	if _, ok := got["unconfined"]; ok {
		t.Error("a skill with no declared network must not get a deny-all role")
	}
}

// The role name has to match what the invoker sets on the subprocess,
// or the ACL applies to nobody.
func TestSkillRoleNameMatchesTheInvoker(t *testing.T) {
	reg := skills.NewRegistry(slog.New(slog.DiscardHandler))
	reg.Put(skillWithNetwork("weather", []string{"wttr.in"}))
	n := &Node{skillRegistry: reg, log: slog.New(slog.DiscardHandler)}

	rules := egress.Build(egress.ACLInputs{SkillNetworks: skillNetworks(n)})
	if hosts, ok := rules.Roles["skill/weather"]; !ok || len(hosts) != 1 {
		t.Errorf("rules.Roles[skill/weather] = %v (present=%v), want [wttr.in]", hosts, ok)
	}
}

func TestSkillNetworksToleratesNoRegistry(t *testing.T) {
	n := &Node{log: slog.New(slog.DiscardHandler)}
	if got := skillNetworks(n); len(got) != 0 {
		t.Errorf("want empty, got %v", got)
	}
}
