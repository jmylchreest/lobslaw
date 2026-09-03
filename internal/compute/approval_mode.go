package compute

import (
	"context"
	"fmt"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/commandrisk"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Which tiers are worth asking about.
//
// The classifier says what a command does; this says what to do with
// that. Deliberately a small closed set rather than a per-tier switch,
// because the thing being chosen is a POSTURE, and an operator who has
// to assemble one out of four booleans will assemble one nobody
// reviewed.
//
// Expressed as POLICY RULES rather than as a branch in the gate, for
// the reason ShellApprovalDefault is: it composes. A mode's rules sit
// at the same floor priority as the rule they sit in front of, so
// anything an operator wrote and anything an approval minted still
// outranks them, and `lobslaw policy list` shows the whole posture in
// one place rather than half of it being invisible behaviour in Go.

// ApprovalMode is a NAMED SET of labels, not a point on a scale.
//
// The tiers this replaces were ranked, so an approved set had to be a
// PREFIX of that ranking: you could have reads, or reads and writes,
// but not reads and deletes without also taking everything ranked
// between them. Labels are a set, so an operator says exactly what
// they mean and nothing else comes along with it.
type ApprovalMode string

const (
	// ApprovalStrict approves nothing. Every command is asked about,
	// which is what the system did before it could classify anything —
	// kept so an operator who wants it can say so rather than having
	// it taken away.
	ApprovalStrict ApprovalMode = "strict"

	// ApprovalStandard approves reads. The shipped default.
	//
	// Not a loosening of the gate so much as a repair of it: a gate
	// that asks eight times in four minutes is answered by reflex, and
	// a reflex is not consent. Removing the questions nobody needed to
	// be asked is what makes the remaining ones legible.
	ApprovalStandard ApprovalMode = "standard"

	// ApprovalTrusted approves reads and ordinary local writes, and
	// still asks about anything that deletes, disrupts, reaches the
	// network, touches privilege, or could not be read.
	ApprovalTrusted ApprovalMode = "trusted"
)

// DefaultApprovalMode is what a node runs with when nothing says
// otherwise.
const DefaultApprovalMode = ApprovalStandard

// approvalPresets is what each named mode expands to.
//
// commandrisk.LabelUnreadable appears in none of them and must not be added to
// one. A command nobody could read is the case the whole gate exists
// for; approving that class by name would be approving everything.
var approvalPresets = map[ApprovalMode][]commandrisk.RiskLabel{
	ApprovalStrict:   {},
	ApprovalStandard: {commandrisk.LabelReads},
	ApprovalTrusted:  {commandrisk.LabelReads, commandrisk.LabelWrites},
}

// Valid reports whether m is one of the shipped presets.
func (m ApprovalMode) Valid() bool {
	_, ok := approvalPresets[m]
	return ok
}

// ApprovedLabels resolves an operator's setting into the set of labels
// that run without asking.
//
// Accepts either a preset name or an explicit list:
//
//	approval_mode = "trusted"
//	approval_mode = ["reads", "writes", "deletes"]
//
// An explicit list is not second-class. It is how a deployment says
// something the three presets cannot — "deletion inside this
// throwaway box is fine, but I still want to hear about egress" — and
// the presets are sugar for the common shapes rather than the only
// vocabulary.
//
// An unrecognised entry is an ERROR rather than a silent drop. A typo
// that quietly approved less would be merely annoying; one that
// quietly approved nothing looks exactly like the gate working, and
// this codebase has spent enough time finding config that was
// discarded without a word.
func ApprovedLabels(setting []string) (map[commandrisk.RiskLabel]bool, error) {
	trimmed := make([]string, 0, len(setting))
	for _, s := range setting {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			trimmed = append(trimmed, s)
		}
	}
	if len(trimmed) == 0 {
		return labelSet(approvalPresets[DefaultApprovalMode]), nil
	}
	// One entry that names a preset expands to it. Two entries never
	// do: ["standard", "deletes"] is a category error rather than a
	// shorthand, and guessing which the operator meant is worse than
	// telling them.
	if len(trimmed) == 1 {
		if preset, ok := approvalPresets[ApprovalMode(trimmed[0])]; ok {
			return labelSet(preset), nil
		}
	}
	out := map[commandrisk.RiskLabel]bool{}
	for _, s := range trimmed {
		l := commandrisk.RiskLabel(s)
		if !l.Valid() {
			return labelSet(approvalPresets[DefaultApprovalMode]),
				fmt.Errorf("unknown approval_mode entry %q (want a preset %q/%q/%q, or labels from %s)",
					s, ApprovalStrict, ApprovalStandard, ApprovalTrusted, commandrisk.RenderLabels(commandrisk.AllRiskLabels))
		}
		if l == commandrisk.LabelUnreadable {
			return labelSet(approvalPresets[DefaultApprovalMode]),
				fmt.Errorf("approval_mode cannot approve %q: a command nobody could read is the case the gate exists for", l)
		}
		out[l] = true
	}
	return out, nil
}

// labelSet turns a slice into the membership map the gate uses.
func labelSet(labels []commandrisk.RiskLabel) map[commandrisk.RiskLabel]bool {
	out := make(map[commandrisk.RiskLabel]bool, len(labels))
	for _, l := range labels {
		out[l] = true
	}
	return out
}

// SortedLabels renders a set deterministically, for logging and
// for building a rule's condition value.
func SortedLabels(set map[commandrisk.RiskLabel]bool) []commandrisk.RiskLabel {
	out := make([]commandrisk.RiskLabel, 0, len(set))
	for _, l := range commandrisk.AllRiskLabels {
		if set[l] {
			out = append(out, l)
		}
	}
	return out
}

// CommandRiskCondition is the policy condition key a rule uses to name
// the labels it approves:
//
//	conditions = [{ key = "command_risk", op = "in", value = "reads,writes" }]
const CommandRiskCondition = "command_risk"

// EvaluateCommandRisk is the condition evaluator for CommandRiskCondition.
//
// A SUBSET CHECK: the condition holds when EVERY label the command
// carries is named in the value. Not "the tier is one of these" —
// there is no tier, and a command that reads and deletes is not
// approved by a rule naming only reads.
//
// Registered on the policy engine at wiring time. Until this existed
// no evaluator was registered anywhere in the tree, and
// Engine.UnevaluableRules reported every conditioned rule as a defect
// — so this must be registered BEFORE that audit runs or a working
// rule is logged as broken at every boot.
//
// A request carrying no labels yields (false, nil): "this rule does
// not apply". Deliberately not an error, because an error on a
// restrictive rule applies it anyway (see Engine.Evaluate), and a
// memory write is not a shell command that failed to classify — it is
// a different question entirely.
func EvaluateCommandRisk(ctx context.Context, cond types.Condition) (bool, error) {
	labels, ok := CommandLabelsFrom(ctx)
	if !ok || len(labels) == 0 {
		return false, nil
	}
	want := parseLabelList(cond.Value)
	if len(want) == 0 {
		return false, fmt.Errorf("condition %q names no labels in its value %q",
			CommandRiskCondition, cond.Value)
	}
	subset := true
	for _, l := range labels {
		if !want[l] {
			subset = false
			break
		}
	}
	switch strings.ToLower(strings.TrimSpace(cond.Op)) {
	case "in", "":
		return subset, nil
	case "not_in":
		return !subset, nil
	default:
		// Fail-closed on an operator's typo rather than guessing: an
		// unknown operator on an allow rule must not be read as "in".
		return false, fmt.Errorf("condition %q does not support op %q", CommandRiskCondition, cond.Op)
	}
}

// parseLabelList reads a comma-separated label list, dropping anything
// outside the closed set.
func parseLabelList(value string) map[commandrisk.RiskLabel]bool {
	out := map[commandrisk.RiskLabel]bool{}
	for _, part := range strings.Split(value, ",") {
		if l := commandrisk.RiskLabel(strings.ToLower(strings.TrimSpace(part))); l.Valid() {
			out[l] = true
		}
	}
	return out
}

// ApprovalModeDefaults is the rule an approved-label set installs.
//
// It must be evaluated BEFORE ShellApprovalDefault, and the engine
// walks its defaults in slice order — so callers append this first.
// Ordering rather than priority because both sit at the same floor,
// and a default that could outrank an operator's rule by winning a
// priority race would not be a default.
//
// An empty approved set returns nothing, which is the same shape a
// node had before modes existed rather than a rule saying "ask" that
// duplicates the one below it.
func ApprovalModeDefaults(approved map[commandrisk.RiskLabel]bool) []types.PolicyRule {
	labels := SortedLabels(approved)
	if len(labels) == 0 {
		return nil
	}
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, string(l))
	}
	return []types.PolicyRule{{
		ID:       "config:compute.approval_mode",
		Subject:  "*",
		Action:   ShellAction,
		Resource: "*",
		Effect:   types.EffectAllow,
		Conditions: []types.Condition{{
			Key:   CommandRiskCondition,
			Op:    "in",
			Value: strings.Join(names, ","),
		}},
		Priority: -1 << 30,
	}}
}
