package main

import (
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

func env(cfg *config.Config) doctorEnv { return doctorEnv{cfg: cfg, cfgPath: "/tmp/config.toml"} }

// The failure these three checks exist for is silence: each
// misconfiguration below looks correct and does nothing.
func TestDoctorIdentityAliasTypo(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Identity: config.IdentityConfig{Aliases: map[string]string{"tg-@alice": "alise"}},
		Users:    []config.UserConfig{{ID: "alice"}},
	}
	_, err := env(cfg).checkIdentityAliases()
	if err == nil {
		t.Fatal("an alias pointing at an undeclared id passed; a typo here silently splits a person's history")
	}
	if !strings.Contains(err.Error(), "alise") {
		t.Errorf("error should name the suspect target: %v", err)
	}
}

func TestDoctorIdentityAliasesReported(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Identity: config.IdentityConfig{Aliases: map[string]string{"tg-@alice": "alice"}},
		Users:    []config.UserConfig{{ID: "alice"}},
	}
	detail, err := env(cfg).checkIdentityAliases()
	if err != nil {
		t.Fatalf("a correct alias failed: %v", err)
	}
	// Printing the map is most of this check's value — an operator
	// cannot spot a wrong alias they were never shown.
	if !strings.Contains(detail, "tg-@alice → alice") {
		t.Errorf("detail should show the resolved map, got %q", detail)
	}
}

func TestDoctorRolesUnreachableWithoutAlias(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Users: []config.UserConfig{{ID: "alice", Roles: []string{"operator"}}},
	}
	_, err := env(cfg).checkUserRolesReachable()
	if err == nil {
		t.Fatal("roles with no alias passed; they apply on JWT channels only and nowhere else")
	}
	if !strings.Contains(err.Error(), "alice") {
		t.Errorf("error should name the user: %v", err)
	}
}

func TestDoctorRolesReachableViaAlias(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Identity: config.IdentityConfig{Aliases: map[string]string{"tg-@alice": "alice"}},
		Users:    []config.UserConfig{{ID: "alice", Roles: []string{"operator"}}},
	}
	if _, err := env(cfg).checkUserRolesReachable(); err != nil {
		t.Fatalf("an aliased user with roles failed: %v", err)
	}
}

func TestDoctorRolesReachableViaChannelBinding(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Users: []config.UserConfig{{
			ID:       "alice",
			Roles:    []string{"operator"},
			Channels: []config.UserChannelAddrConfig{{Type: "telegram", Address: "123456789"}},
		}},
	}
	if _, err := env(cfg).checkUserRolesReachable(); err != nil {
		t.Fatalf("a user bound by channel address failed: %v", err)
	}
}

func TestDoctorOperatorRoleWithoutGrant(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Users: []config.UserConfig{{ID: "alice", Roles: []string{"operator"}}},
	}
	_, err := env(cfg).checkOperatorGrant()
	if err == nil {
		t.Fatal("role:operator with no granting rule passed; the role confers nothing on its own")
	}
}

// The other direction: a grant nobody holds is a dead rule, and looks
// just as much like the feature is switched on.
func TestDoctorOperatorGrantWithoutHolder(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Policy: config.PolicyConfig{Rules: []config.PolicyRuleConfig{
			{Subject: "role:operator", Action: "memory:read:any", Effect: "allow"},
		}},
	}
	_, err := env(cfg).checkOperatorGrant()
	if err == nil {
		t.Fatal("a grant with no holder passed; the rule is dead")
	}
}

func TestDoctorOperatorFullyWired(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Users: []config.UserConfig{{ID: "alice", Roles: []string{"operator"}}},
		Policy: config.PolicyConfig{Rules: []config.PolicyRuleConfig{
			{Subject: "role:operator", Action: "memory:read:any", Effect: "allow"},
		}},
	}
	if _, err := env(cfg).checkOperatorGrant(); err != nil {
		t.Fatalf("a fully wired operator failed: %v", err)
	}
}

// Neither half declared is the common case and must stay quiet.
func TestDoctorNoOperatorIsFine(t *testing.T) {
	t.Parallel()
	if _, err := env(&config.Config{}).checkOperatorGrant(); err != nil {
		t.Fatalf("a deployment with no operator failed: %v", err)
	}
}
