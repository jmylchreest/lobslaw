package commandrisk

import (
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

// The catalogue ships as data, so "does it parse" is no longer a
// compile error and has to be a test.
func TestEmbeddedCatalogueLoads(t *testing.T) {
	if len(DefaultCommandRisks) < 200 {
		t.Fatalf("catalogue has %d entries; it shipped with 237", len(DefaultCommandRisks))
	}
	for name, r := range DefaultCommandRisks {
		if len(r.Labels) == 0 && len(r.Sub) == 0 && len(r.FlagSub) == 0 {
			t.Errorf("%q has no labels and no verbs, so it silently removes a command", name)
		}
		for _, l := range r.Labels {
			if !l.Valid() {
				t.Errorf("%q carries invalid label %q", name, l)
			}
		}
	}
}

// A typo must fail the build rather than being dropped. A dropped
// "deletes" leaves the command classified as whatever remained, which
// is the one error this subsystem exists to prevent.
func TestUnknownLabelIsRefused(t *testing.T) {
	_, err := ParseTable([]byte(`[commands.foo]
labels = ["delete"]`)) // singular: the plausible typo
	if err == nil {
		t.Fatal("an unknown label parsed without complaint")
	}
	if !strings.Contains(err.Error(), "delete") {
		t.Errorf("error does not name the offending label: %v", err)
	}
}

// extends exists so a family cannot drift apart.
func TestExtendsInheritsAndOverrides(t *testing.T) {
	table, err := ParseTable([]byte(`
[commands.pacman]
labels = ["reads"]
flag_subcommands = { "-S" = ["network", "writes"], "-Q" = ["reads"] }

[commands.paru]
extends = "pacman"
flag_subcommands = { "-Ss" = ["reads", "network"] }
`))
	if err != nil {
		t.Fatal(err)
	}
	paru := table["paru"]
	// Inherited.
	if got := paru.FlagSub["-Q"]; !HasLabel(got, LabelReads) {
		t.Errorf("paru -Q = %v; the parent's verbs must carry over", got)
	}
	// Added without restating the rest.
	if got := paru.FlagSub["-Ss"]; !HasLabel(got, LabelNetwork) {
		t.Errorf("paru -Ss = %v; the child's own verbs must apply", got)
	}
	if got := paru.Labels; !HasLabel(got, LabelReads) {
		t.Errorf("paru labels = %v; base labels must carry over", got)
	}
}

func TestExtendsUnknownParentIsRefused(t *testing.T) {
	if _, err := ParseTable([]byte("[commands.foo]\nextends = \"nope\"")); err == nil {
		t.Fatal("extending a parent that does not exist parsed without complaint")
	}
}

// The reason the two types were unified: config could not express a
// flag-driven package manager at all, because the config struct was a
// hand-maintained mirror that never grew FlagSubcommands. Anything the
// shipped catalogue can say, a deployment can now say too.
func TestConfigCanExpressEveryShippedField(t *testing.T) {
	rule, err := RuleFromConfig("mypkg", config.CommandRiskConfig{
		Labels:          []string{"reads"},
		FlagSubcommands: map[string][]string{"-i": {"network", "privilege", "writes"}},
		OperandLabels:   []string{"writes"},
		TargetLast:      true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rule.FlagSub["-i"]; !HasLabel(got, LabelPrivilege) {
		t.Errorf("flag_subcommands did not survive config: %v", got)
	}
	if !HasLabel(rule.OperandLabels, LabelWrites) || !rule.TargetLast {
		t.Errorf("operand_labels/target_last did not survive config: %+v", rule)
	}
}

// Config may extend a shipped entry, so a deployment that wraps pacman
// does not restate thirty flags.
func TestConfigCanExtendAShippedEntry(t *testing.T) {
	rule, err := RuleFromConfig("mywrapper", config.CommandRiskConfig{Extends: "pacman"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rule.FlagSub) == 0 {
		t.Fatal("extending a shipped entry inherited none of its verbs")
	}
}
