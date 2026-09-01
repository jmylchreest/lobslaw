package compute

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// A mode is only worth having if it actually changes what runs unasked,
// so these go through a real engine with the real rules in the real
// order rather than asserting on the rule structs. The ordering in
// particular is the kind of thing that works by accident: the engine
// appends its defaults AFTER sorting, so slice order is the tiebreak
// between an allow and the require_confirmation it sits in front of.

func modeGatedExecutor(t *testing.T, mode ApprovalMode, rules ...*lobslawv1.PolicyRule) (*Executor, *SessionApprovals) {
	t.Helper()
	dir := t.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(dir, "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rules = append(rules, &lobslawv1.PolicyRule{
		Id: "lobslaw-builtin-shell_command", Subject: "*",
		Action: "tool:exec", Resource: "shell_command",
		Effect: "allow", Priority: 1,
	})
	for _, r := range rules {
		raw, err := proto.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(memory.BucketPolicyRules, r.Id, raw); err != nil {
			t.Fatal(err)
		}
	}

	eng := policy.NewEngine(store, slog.New(slog.DiscardHandler))
	// Exactly what wireApprovalGates does, in the order it does it.
	eng.RegisterCondition(CommandRiskCondition, EvaluateCommandRisk)
	defaults := append(ApprovalModeDefaults(mode), ShellApprovalDefault(),
		// The reaching-off-the-box defaults go in for the same reason
		// wireApprovalGates installs them whenever the shell is
		// registered: `curl` from shell_command resolves to net:fetch,
		// and without a rule for that action it hits default-deny —
		// a refusal nobody wrote.
		RemoteApprovalDefault(), RemoteCopyApprovalDefault(), NetFetchApprovalDefault())
	eng.SetDefaults(defaults)

	approvals := NewSessionApprovals()
	e := NewExecutor(newTestCatalogue(), eng, nil, ExecutorConfig{}, slog.New(slog.DiscardHandler))
	e.SetSessionApprovals(approvals)
	e.RequireCommandApproval("shell_command", ShellGrantResource, ShellCommandSummary)
	return e, approvals
}

func TestApprovalModeDecidesWhatRunsUnasked(t *testing.T) {
	// One command per tier, all of them shapes an environment probe
	// actually produces.
	const (
		readCmd        = "uname -a"
		writeCmd       = "touch /tmp/.probe"
		networkCmd     = "curl -sS https://example.com"
		destructiveCmd = "rm -rf /etc/hosts"
		unknownCmd     = "for b in node bun; do command -v $b; done"
	)

	tests := []struct {
		mode ApprovalMode
		// asked lists the commands that must still raise a
		// confirmation under this mode.
		allowed []string
		asked   []string
	}{
		{
			mode:    ApprovalStrict,
			allowed: nil,
			asked:   []string{readCmd, writeCmd, networkCmd, destructiveCmd, unknownCmd},
		},
		{
			mode:    ApprovalStandard,
			allowed: []string{readCmd},
			asked:   []string{writeCmd, networkCmd, destructiveCmd, unknownCmd},
		},
		{
			mode:    ApprovalTrusted,
			allowed: []string{readCmd, writeCmd},
			asked:   []string{networkCmd, destructiveCmd, unknownCmd},
		},
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			t.Parallel()
			e, _ := modeGatedExecutor(t, tt.mode)
			for _, cmd := range tt.allowed {
				if err := checkShell(context.Background(), t, e, cmd); err != nil {
					t.Errorf("%q was asked about under %s: %v", cmd, tt.mode, err)
				}
			}
			for _, cmd := range tt.asked {
				err := checkShell(context.Background(), t, e, cmd)
				if !errors.Is(err, ErrRequireConfirm) {
					t.Errorf("%q ran unasked under %s: %v", cmd, tt.mode, err)
				}
			}
		})
	}
}

// No mode waves through the network, a deletion, or something nobody
// could read. If this ever passes for one of them, the mode table has
// grown a tier it should not have.
func TestNoModeAllowsTheDangerousTiers(t *testing.T) {
	t.Parallel()
	for _, mode := range []ApprovalMode{ApprovalStrict, ApprovalStandard, ApprovalTrusted} {
		for _, tier := range mode.AutoAllowed() {
			switch tier {
			case RiskNetwork, RiskDestructive, RiskUnknown:
				t.Errorf("mode %q auto-allows %q", mode, tier)
			}
		}
	}
}

// An operator's rule outranks the mode, in both directions: the mode
// sits at the floor priority so anything written down beats it.
func TestAnOperatorRuleOutranksTheMode(t *testing.T) {
	t.Parallel()
	e, _ := modeGatedExecutor(t, ApprovalStandard, &lobslawv1.PolicyRule{
		Id: "operator-no-uname", Subject: "*",
		Action: ShellAction, Resource: "uname -a",
		Effect: "require_confirmation", Priority: 10,
	})
	if err := checkShell(context.Background(), t, e, "uname -a"); !errors.Is(err, ErrRequireConfirm) {
		t.Fatalf("the mode's allow beat an operator rule: %v", err)
	}
}

// The grant a channel records when somebody taps "Allow read-only
// here": it covers the tier, not the command, so the NEXT command —
// a different one, which is what a probing agent produces — is not
// asked about either.
func TestATierGrantCoversTheNextCommandToo(t *testing.T) {
	t.Parallel()
	e, approvals := modeGatedExecutor(t, ApprovalStrict)
	ctx := turn.WithIdentity(context.Background(), turn.Identity{
		Channel: "telegram", ChannelID: "42",
	})

	if err := checkShell(ctx, t, e, "uname -a"); !errors.Is(err, ErrRequireConfirm) {
		t.Fatalf("strict mode did not ask: %v", err)
	}
	if !approvals.Grant(ctx, ShellAction, RiskGrantResource(RiskRead)) {
		t.Fatal("the tier grant was not recorded")
	}
	// A DIFFERENT read command. The per-command grant could never have
	// covered this one, which is the entire reason the tier grant
	// exists.
	if err := checkShell(ctx, t, e, "df -h /"); err != nil {
		t.Errorf("a second read command was asked about: %v", err)
	}
	// The grant is bounded by its tier: a write is still asked about.
	if err := checkShell(ctx, t, e, "touch /tmp/.probe"); !errors.Is(err, ErrRequireConfirm) {
		t.Errorf("a read-only grant covered a write: %v", err)
	}
}

// A tier grant given in one conversation must not answer for another.
func TestATierGrantIsScopedToItsConversation(t *testing.T) {
	t.Parallel()
	e, approvals := modeGatedExecutor(t, ApprovalStrict)
	here := turn.WithIdentity(context.Background(), turn.Identity{
		Channel: "telegram", ChannelID: "42",
	})
	elsewhere := turn.WithIdentity(context.Background(), turn.Identity{
		Channel: "telegram", ChannelID: "99",
	})

	approvals.Grant(here, ShellAction, RiskGrantResource(RiskRead))
	if err := checkShell(elsewhere, t, e, "uname -a"); !errors.Is(err, ErrRequireConfirm) {
		t.Errorf("a grant leaked into another conversation: %v", err)
	}
}

func TestEvaluateCommandRisk(t *testing.T) {
	t.Parallel()
	cond := func(op, value string) types.Condition {
		return types.Condition{Key: CommandRiskCondition, Op: op, Value: value}
	}
	read := WithCommandRisk(context.Background(), RiskRead)

	tests := []struct {
		name    string
		ctx     context.Context
		cond    types.Condition
		want    bool
		wantErr bool
	}{
		{"matches its tier", read, cond("in", "read"), true, false},
		{"matches within a list", read, cond("in", "read,write"), true, false},
		{"does not match another tier", read, cond("in", "write"), false, false},
		{"an empty op reads as in", read, cond("", "read"), true, false},
		{"not_in inverts", read, cond("not_in", "write"), true, false},
		{"not_in excludes", read, cond("not_in", "read"), false, false},
		// Not an error: a memory write is a different question, not a
		// shell command that failed to classify. An error here would
		// APPLY a restrictive rule rather than skip it.
		{"an unclassified request does not match", context.Background(), cond("in", "read"), false, false},
		// Fail-closed on an operator's typo rather than guessing.
		{"an unknown op errors", read, cond("above", "read"), false, true},
		{"an empty value errors", read, cond("in", ""), false, true},
		{"a value of only junk errors", read, cond("in", "bananas"), false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EvaluateCommandRisk(tt.ctx, tt.cond)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseApprovalMode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    ApprovalMode
		wantErr bool
	}{
		{"", DefaultApprovalMode, false},
		{"strict", ApprovalStrict, false},
		{"STANDARD", ApprovalStandard, false},
		{"  trusted  ", ApprovalTrusted, false},
		// A typo must not quietly select a posture nobody chose, and
		// the fallback is the shipped default rather than the loosest.
		{"trused", DefaultApprovalMode, true},
		{"yolo", DefaultApprovalMode, true},
	}
	for _, tt := range tests {
		got, err := ParseApprovalMode(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseApprovalMode(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
		}
		if got != tt.want {
			t.Errorf("ParseApprovalMode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Strict is the ABSENCE of the mode rules, not a second rule restating
// the confirmation default — otherwise switching to strict would leave
// two rules saying the same thing and one of them unexplained.
func TestStrictInstallsNoRules(t *testing.T) {
	t.Parallel()
	if got := ApprovalModeDefaults(ApprovalStrict); len(got) != 0 {
		t.Errorf("strict installed %d rules, want 0", len(got))
	}
	if got := ApprovalModeDefaults(ApprovalStandard); len(got) != 1 {
		t.Fatalf("standard installed %d rules, want 1", len(got))
	}
	rule := ApprovalModeDefaults(ApprovalStandard)[0]
	if rule.Effect != types.EffectAllow || rule.Action != ShellAction {
		t.Errorf("rule = %s/%s, want allow/%s", rule.Effect, rule.Action, ShellAction)
	}
	if len(rule.Conditions) != 1 || rule.Conditions[0].Key != CommandRiskCondition {
		t.Errorf("rule conditions = %+v, want one %q", rule.Conditions, CommandRiskCondition)
	}
	// At the floor, so an operator's rule always outranks it.
	if rule.Priority != -1<<30 {
		t.Errorf("priority = %d, want the floor", rule.Priority)
	}
}
