package compute

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/commandrisk"

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

func modeGatedExecutor(t *testing.T, approve []string, rules ...*lobslawv1.PolicyRule) (*Executor, *SessionApprovals) {
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
	approved, err := ApprovedLabels(approve)
	if err != nil {
		t.Fatal(err)
	}
	defaults := append(ApprovalModeDefaults(approved), ShellApprovalDefault(),
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
			e, _ := modeGatedExecutor(t, []string{string(tt.mode)})
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
func TestNoPresetApprovesTheDangerousLabels(t *testing.T) {
	t.Parallel()
	for _, mode := range []ApprovalMode{ApprovalStrict, ApprovalStandard, ApprovalTrusted} {
		approved, err := ApprovedLabels([]string{string(mode)})
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range []commandrisk.RiskLabel{commandrisk.LabelDeletes, commandrisk.LabelDisrupts, commandrisk.LabelNetwork, commandrisk.LabelPrivilege, commandrisk.LabelUnreadable} {
			if approved[l] {
				t.Errorf("preset %q approves %q", mode, l)
			}
		}
	}
}

// An operator's rule outranks the mode, in both directions: the mode
// sits at the floor priority so anything written down beats it.
func TestAnOperatorRuleOutranksTheMode(t *testing.T) {
	t.Parallel()
	e, _ := modeGatedExecutor(t, []string{"standard"}, &lobslawv1.PolicyRule{
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
	e, approvals := modeGatedExecutor(t, []string{"strict"})
	ctx := turn.WithIdentity(context.Background(), turn.Identity{
		Channel: "telegram", ChannelID: "42",
	})

	if err := checkShell(ctx, t, e, "uname -a"); !errors.Is(err, ErrRequireConfirm) {
		t.Fatalf("strict mode did not ask: %v", err)
	}
	if !approvals.Grant(ctx, ShellAction, RiskGrantResource(commandrisk.LabelReads)) {
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
	e, approvals := modeGatedExecutor(t, []string{"strict"})
	here := turn.WithIdentity(context.Background(), turn.Identity{
		Channel: "telegram", ChannelID: "42",
	})
	elsewhere := turn.WithIdentity(context.Background(), turn.Identity{
		Channel: "telegram", ChannelID: "99",
	})

	approvals.Grant(here, ShellAction, RiskGrantResource(commandrisk.LabelReads))
	if err := checkShell(elsewhere, t, e, "uname -a"); !errors.Is(err, ErrRequireConfirm) {
		t.Errorf("a grant leaked into another conversation: %v", err)
	}
}

func TestEvaluateCommandRisk(t *testing.T) {
	t.Parallel()
	cond := func(op, value string) types.Condition {
		return types.Condition{Key: CommandRiskCondition, Op: op, Value: value}
	}
	read := WithCommandLabels(context.Background(), commandrisk.L(commandrisk.LabelReads))

	tests := []struct {
		name    string
		ctx     context.Context
		cond    types.Condition
		want    bool
		wantErr bool
	}{
		{"matches its tier", read, cond("in", "reads"), true, false},
		{"matches within a list", read, cond("in", "reads,writes"), true, false},
		{"does not match another tier", read, cond("in", "writes"), false, false},
		{"an empty op reads as in", read, cond("", "reads"), true, false},
		{"not_in inverts", read, cond("not_in", "writes"), true, false},
		{"not_in excludes", read, cond("not_in", "reads"), false, false},
		// Not an error: a memory write is a different question, not a
		// shell command that failed to classify. An error here would
		// APPLY a restrictive rule rather than skip it.
		{"an unclassified request does not match", context.Background(), cond("in", "reads"), false, false},
		// Fail-closed on an operator's typo rather than guessing.
		{"an unknown op errors", read, cond("above", "reads"), false, true},
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

func TestApprovedLabels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      []string
		want    []commandrisk.RiskLabel
		wantErr bool
	}{
		{"unset takes the default", nil, commandrisk.L(commandrisk.LabelReads), false},
		{"a preset expands", []string{"trusted"}, commandrisk.L(commandrisk.LabelReads, commandrisk.LabelWrites), false},
		{"strict approves nothing", []string{"strict"}, nil, false},
		{"case and space are forgiven", []string{"  STANDARD "}, commandrisk.L(commandrisk.LabelReads), false},
		// The thing presets cannot say.
		{"an explicit set", []string{"reads", "writes", "deletes"}, commandrisk.L(commandrisk.LabelReads, commandrisk.LabelWrites, commandrisk.LabelDeletes), false},
		// A typo must not quietly approve the wrong thing, in either
		// direction.
		{"an unknown label errors", []string{"reads", "delete"}, commandrisk.L(commandrisk.LabelReads), true},
		{"a preset mixed with labels errors", []string{"standard", "deletes"}, commandrisk.L(commandrisk.LabelReads), true},
		// Never, by any spelling.
		{"unreadable is refused", []string{"reads", "unreadable"}, commandrisk.L(commandrisk.LabelReads), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ApprovedLabels(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				// The fallback is the shipped default, not the loosest.
				if !got[commandrisk.LabelReads] || got[commandrisk.LabelWrites] {
					t.Errorf("on error the fallback was %v, want the default", SortedLabels(got))
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", SortedLabels(got), tt.want)
			}
			for _, l := range tt.want {
				if !got[l] {
					t.Errorf("got %v, want %v", SortedLabels(got), tt.want)
				}
			}
		})
	}
}

// Strict is the ABSENCE of the mode rules, not a second rule restating
// the confirmation default — otherwise switching to strict would leave
// two rules saying the same thing and one of them unexplained.
func TestStrictInstallsNoRules(t *testing.T) {
	t.Parallel()
	strict, _ := ApprovedLabels([]string{"strict"})
	if got := ApprovalModeDefaults(strict); len(got) != 0 {
		t.Errorf("strict installed %d rules, want 0", len(got))
	}
	standard, _ := ApprovedLabels([]string{"standard"})
	rules := ApprovalModeDefaults(standard)
	if len(rules) != 1 {
		t.Fatalf("standard installed %d rules, want 1", len(rules))
	}
	rule := rules[0]
	if rule.Effect != types.EffectAllow || rule.Action != ShellAction {
		t.Errorf("rule = %s/%s, want allow/%s", rule.Effect, rule.Action, ShellAction)
	}
	if len(rule.Conditions) != 1 || rule.Conditions[0].Key != CommandRiskCondition {
		t.Errorf("rule conditions = %+v, want one %q", rule.Conditions, CommandRiskCondition)
	}
	if rule.Conditions[0].Value != "reads" {
		t.Errorf("condition value = %q, want reads", rule.Conditions[0].Value)
	}
	// At the floor, so an operator's rule always outranks it.
	if rule.Priority != -1<<30 {
		t.Errorf("priority = %d, want the floor", rule.Priority)
	}
}
