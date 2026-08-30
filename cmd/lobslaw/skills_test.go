package main

import (
	"strings"
	"testing"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// The identity comes from the MANIFEST, not from flags. Both are
// already stated in the file, and two sources for one fact eventually
// disagree — an operator who typed a version that did not match would
// install a record describing a skill that is not the one in it.

func TestIdentityComesFromTheManifest(t *testing.T) {
	t.Parallel()
	name, version, err := manifestIdentity([]byte(
		"name: tidy\nversion: 1.2.3\nruntime: python\nhandler: handler.py\n"))
	if err != nil {
		t.Fatal(err)
	}
	if name != "tidy" || version != "1.2.3" {
		t.Errorf("got %q %q", name, version)
	}
}

// Comments, quotes and an unusual key order are all ordinary in a
// hand-written manifest.
func TestIdentitySurvivesRealisticFormatting(t *testing.T) {
	t.Parallel()
	name, version, err := manifestIdentity([]byte(
		"# published by example.com\n\nversion: \"1.2.3\"\nname: 'tidy'\nruntime: python\n"))
	if err != nil {
		t.Fatal(err)
	}
	if name != "tidy" || version != "1.2.3" {
		t.Errorf("got %q %q", name, version)
	}
}

// A NESTED name is a credential provider or a bundled binary, not the
// skill. Taking the first "name:" anywhere would install a skill
// called "github".
func TestANestedNameIsNotTheSkillName(t *testing.T) {
	t.Parallel()
	// The nested block comes FIRST, which is valid YAML and is the only
	// arrangement that actually exercises the indent check — with the
	// top-level keys first, first-wins hides the bug.
	manifest := "credentials:\n  - provider: github\n    name: not-the-skill\n" +
		"binaries:\n  - name: helper-bin\n    version: 9.9.9\n" +
		"name: tidy\nversion: 1.2.3\n"
	name, version, err := manifestIdentity([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	if name != "tidy" {
		t.Errorf("name = %q; a nested key won", name)
	}
	if version != "1.2.3" {
		t.Errorf("version = %q; a nested key won", version)
	}
}

func TestAManifestMissingEitherFieldIsRefused(t *testing.T) {
	t.Parallel()
	for _, manifest := range []string{
		"version: 1.2.3\nruntime: python\n",
		"name: tidy\nruntime: python\n",
		"",
	} {
		if _, _, err := manifestIdentity([]byte(manifest)); err == nil {
			t.Errorf("%q was accepted", manifest)
		}
	}
}

// --- tiers -------------------------------------------------------------

// `agent` means "the agent wrote this". Letting a person claim it from
// a command line would make provenance a thing anybody can assert
// rather than a fact about where a skill came from.
func TestTheAgentTierIsNotInstallable(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"agent", "AGENT", "dev", "nonsense", ""} {
		if _, err := parseTier(in); err == nil {
			t.Errorf("tier %q was accepted", in)
		}
	}
}

func TestInstallableTiersParse(t *testing.T) {
	t.Parallel()
	// A slice rather than a map, because one of these deliberately has
	// surrounding whitespace — an operator's shell will hand it over
	// eventually — and that reads as a typo in a map key.
	cases := []struct {
		in   string
		want lobslawv1.SkillTier
	}{
		{"operator", lobslawv1.SkillTier_SKILL_TIER_OPERATOR},
		{"OPERATOR", lobslawv1.SkillTier_SKILL_TIER_OPERATOR},
		{" signed ", lobslawv1.SkillTier_SKILL_TIER_SIGNED},
	}
	for _, c := range cases {
		in, want := c.in, c.want
		got, err := parseTier(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %v, want %v", in, got, want)
		}
	}
}

// The refusal names what IS installable, or the operator is left
// guessing at a vocabulary.
func TestTheTierRefusalNamesTheChoices(t *testing.T) {
	t.Parallel()
	_, err := parseTier("agent")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "operator") || !strings.Contains(err.Error(), "signed") {
		t.Errorf("err = %q; it does not say what is installable", err)
	}
}

func TestTierLabelsRoundTrip(t *testing.T) {
	t.Parallel()
	for _, tier := range []lobslawv1.SkillTier{
		lobslawv1.SkillTier_SKILL_TIER_OPERATOR,
		lobslawv1.SkillTier_SKILL_TIER_SIGNED,
	} {
		got, err := parseTier(tierLabel(tier))
		if err != nil {
			t.Fatalf("%v: %v", tier, err)
		}
		if got != tier {
			t.Errorf("%v rendered as %q and parsed back as %v", tier, tierLabel(tier), got)
		}
	}
}

// --- discoverability ---------------------------------------------------

// A subcommand the dispatcher accepts but the help does not mention is
// one nobody finds. `rollback` was added after the usage text was
// written, which is exactly when this drifts.
func TestEverySubcommandIsInTheUsage(t *testing.T) {
	t.Parallel()
	for _, sub := range []string{"list", "import", "export", "remove", "rollback"} {
		if !strings.Contains(skillsUsage, "  "+sub) {
			t.Errorf("`skills %s` is dispatched but not in the usage text", sub)
		}
	}
}

// The reverse: the usage must not advertise something that is not
// wired, which sends an operator to a command that exits 2.
func TestTheUsageAdvertisesNothingUnwired(t *testing.T) {
	t.Parallel()
	dispatched := map[string]bool{
		"list": true, "import": true, "export": true,
		"remove": true, "rollback": true,
	}
	for line := range strings.SplitSeq(skillsUsage, "\n") {
		if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
			continue
		}
		verb, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		if verb == "" {
			continue
		}
		if !dispatched[verb] {
			t.Errorf("the usage advertises %q, which the dispatcher does not handle", verb)
		}
	}
}
