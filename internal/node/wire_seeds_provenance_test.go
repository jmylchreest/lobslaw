package node

import (
	"testing"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"

	"github.com/jmylchreest/lobslaw/internal/policy"
)

// What survives a reconcile, and what does not.
//
// The adoption case is the one that matters most. On the boot that
// introduces provenance every stored rule is unstamped, including the
// tool:exec allow for every builtin — reconciling on "no provenance"
// alone would take the agent's entire toolset out in one pass.
func TestReconcileKeepsWhatIsStillJustified(t *testing.T) {
	t.Parallel()

	configIDs := map[string]bool{"op-allow-git": true}
	seedIDs := map[string]bool{"lobslaw-builtin-read_file": true}

	cases := []struct {
		name       string
		id         string
		createdBy  string
		wantRemove bool
		why        string
	}{
		{"a config rule still in config", "op-allow-git", policy.RuleSourceConfig, false,
			"config still asks for it"},
		{"a config rule deleted from config", "op-allow-old", policy.RuleSourceConfig, true,
			"the operator removed it and expects it gone"},
		{"a seed for a registered tool", "lobslaw-builtin-read_file", policy.RuleSourceSeed, false,
			"the tool is still registered"},
		{"a seed for a tool no longer registered", "lobslaw-builtin-gone", policy.RuleSourceSeed, true,
			"nothing it was about exists"},
		{"an approval a person tapped", "approval:01ABC", policy.ApprovalRulePrefix + "01ABC", false,
			"only the person who granted it may revoke it"},

		// Adoption: unstamped, but demonstrably ours.
		{"an unstamped rule matching config", "op-allow-git", "", false,
			"the id is in config, so we wrote it"},
		{"an unstamped builtin seed", "lobslaw-builtin-read_file", "", false,
			"the id is a live seed target, so we wrote it"},

		// Genuinely foreign.
		{"an unstamped rule matching nothing", "left-over-from-a-test", "", true,
			"nothing here would write this today"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			removed := shouldRemoveRule(&lobslawv1.PolicyRule{Id: c.id, CreatedBy: c.createdBy}, configIDs, seedIDs)
			if removed != c.wantRemove {
				t.Errorf("remove = %v, want %v — %s", removed, c.wantRemove, c.why)
			}
		})
	}
}
