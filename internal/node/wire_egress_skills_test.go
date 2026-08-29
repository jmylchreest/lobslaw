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

// strict_skill_egress is opt-in because turning it on breaks every
// skill written before per-skill roles were populated — those manifests
// omit network: entirely. Off, an undeclared skill must stay ABSENT
// from the map (absent = default ACL); on, it must be present and nil
// (nil = the builder's explicit-deny branch).
func TestStrictSkillEgressDeniesUndeclaredSkills(t *testing.T) {
	newNode := func(strict bool) *Node {
		reg := skills.NewRegistry(slog.New(slog.DiscardHandler))
		reg.Put(skillWithNetwork("weather", []string{"wttr.in"}))
		reg.Put(skillWithNetwork("undeclared", nil))
		n := &Node{skillRegistry: reg, log: slog.New(slog.DiscardHandler)}
		n.cfg.Security.StrictSkillEgress = strict
		return n
	}

	lax := skillNetworks(newNode(false))
	if _, present := lax["undeclared"]; present {
		t.Error("lax: an undeclared skill must be absent so it keeps the default ACL")
	}

	strict := skillNetworks(newNode(true))
	hosts, present := strict["undeclared"]
	if !present {
		t.Fatal("strict: an undeclared skill must be present so the builder denies it")
	}
	if hosts != nil {
		t.Errorf("strict: hosts = %v, want nil", hosts)
	}
	// A declared skill is unaffected by the switch either way.
	if len(strict["weather"]) != 1 || len(lax["weather"]) != 1 {
		t.Error("a declared network must be honoured under both settings")
	}

	// And the builder must turn that nil into a registered deny-all
	// role rather than leaving the skill on the unknown-role path.
	rules := egress.Build(egress.ACLInputs{SkillNetworks: strict})
	if got, ok := rules.Roles["skill/undeclared"]; !ok || len(got) != 0 {
		t.Errorf("rules.Roles[skill/undeclared] = %v (present=%v), want a registered empty role", got, ok)
	}
}
