package compute

import (
	"context"
	"fmt"
	"strings"

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

// ApprovalMode is the shipped posture for shell command approval.
type ApprovalMode string

const (
	// ApprovalStrict asks about every command, whatever it does. What
	// the system did before there was a classifier, kept so an
	// operator who wants it can say so rather than having it taken
	// away.
	ApprovalStrict ApprovalMode = "strict"

	// ApprovalStandard asks about everything except commands that only
	// read. The shipped default.
	//
	// Not a loosening of the gate so much as a repair of it: a gate
	// that asks eight times in four minutes is answered by reflex, and
	// a reflex is not consent. Removing the questions nobody needed to
	// be asked is what makes the remaining ones legible.
	ApprovalStandard ApprovalMode = "standard"

	// ApprovalTrusted additionally allows ordinary local writes, and
	// still asks about anything that reaches the network, deletes,
	// changes the machine, runs as root, or cannot be read.
	//
	// For a node whose shell already runs inside a sandbox it can
	// afford to lose. Note what this does NOT relax: the scratch-path
	// de-escalation in the classifier only ever turns a deletion into
	// a write, so this is the one mode where that de-escalation can
	// cause something to run unasked — which is why it is opt-in and
	// says so in the documentation.
	ApprovalTrusted ApprovalMode = "trusted"
)

// DefaultApprovalMode is what a node runs with when nothing says
// otherwise.
const DefaultApprovalMode = ApprovalStandard

// Valid reports whether m is one of the three.
func (m ApprovalMode) Valid() bool {
	switch m {
	case ApprovalStrict, ApprovalStandard, ApprovalTrusted:
		return true
	}
	return false
}

// AutoAllowed is the tiers this mode runs without asking.
//
// Network, destructive and unknown are absent from every mode, and
// there is no mode that includes them. A posture that waves through a
// command nobody could read is not a posture, and if somebody wants
// one they can write the policy rule and own it.
func (m ApprovalMode) AutoAllowed() []CommandRisk {
	switch m {
	case ApprovalStandard:
		return []CommandRisk{RiskRead}
	case ApprovalTrusted:
		return []CommandRisk{RiskRead, RiskWrite}
	default:
		return nil
	}
}

// ParseApprovalMode reads an operator's setting, reporting whether it
// was recognised. An empty setting takes the default; anything else
// unrecognised is an error rather than a silent fallback, because a
// typo that quietly selected a posture nobody chose is the failure
// this whole area is trying to remove.
func ParseApprovalMode(s string) (ApprovalMode, error) {
	trimmed := ApprovalMode(strings.ToLower(strings.TrimSpace(s)))
	if trimmed == "" {
		return DefaultApprovalMode, nil
	}
	if !trimmed.Valid() {
		return DefaultApprovalMode, fmt.Errorf(
			"unknown approval_mode %q (want %q, %q or %q)",
			s, ApprovalStrict, ApprovalStandard, ApprovalTrusted)
	}
	return trimmed, nil
}

// CommandRiskCondition is the policy condition key a rule uses to
// name a tier:
//
//	conditions = [{ key = "command_risk", op = "in", value = "read,write" }]
const CommandRiskCondition = "command_risk"

// EvaluateCommandRisk is the condition evaluator for CommandRiskCondition.
//
// Registered on the policy engine at wiring time. Until this existed
// no evaluator was registered anywhere in the tree, and
// Engine.UnevaluableRules reported every conditioned rule as a defect
// — so this must be registered BEFORE that audit runs or a working
// rule is logged as broken at every boot.
//
// A request that carries no tier yields (false, nil): "this rule does
// not apply". Deliberately not an error, because an error on a
// restrictive rule applies it anyway (see Engine.Evaluate), and a
// memory write is not a shell command that failed to classify — it is
// a different question entirely.
func EvaluateCommandRisk(ctx context.Context, cond types.Condition) (bool, error) {
	tier, ok := CommandRiskFrom(ctx)
	if !ok {
		return false, nil
	}
	want := parseRiskList(cond.Value)
	if len(want) == 0 {
		return false, fmt.Errorf("condition %q has no tiers in its value %q",
			CommandRiskCondition, cond.Value)
	}
	switch strings.ToLower(strings.TrimSpace(cond.Op)) {
	case "in", "":
		return want[tier], nil
	case "not_in":
		return !want[tier], nil
	default:
		// Fail-closed on an operator's typo rather than guessing: an
		// unknown operator on an allow rule must not be read as "in".
		return false, fmt.Errorf("condition %q does not support op %q", CommandRiskCondition, cond.Op)
	}
}

// parseRiskList reads a comma-separated tier list, dropping anything
// outside the enum.
func parseRiskList(value string) map[CommandRisk]bool {
	out := map[CommandRisk]bool{}
	for _, part := range strings.Split(value, ",") {
		tier := CommandRisk(strings.ToLower(strings.TrimSpace(part)))
		if tier.Valid() {
			out[tier] = true
		}
	}
	return out
}

// ApprovalModeDefaults is the rules a mode installs.
//
// They must be evaluated BEFORE ShellApprovalDefault, and the engine
// walks its defaults in slice order — so callers append these first.
// Ordering rather than priority because both sit at the same floor,
// and a default that could outrank an operator's rule by winning a
// priority race would not be a default.
//
// Strict returns nothing, which is the same shape a node had before
// modes existed rather than a rule saying "ask", so switching to
// strict removes behaviour rather than adding a second rule that
// duplicates the one already there.
func ApprovalModeDefaults(mode ApprovalMode) []types.PolicyRule {
	tiers := mode.AutoAllowed()
	if len(tiers) == 0 {
		return nil
	}
	names := make([]string, 0, len(tiers))
	for _, t := range tiers {
		names = append(names, string(t))
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
