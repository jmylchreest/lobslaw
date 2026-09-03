package commandrisk

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

// Rule is the authored shape of one entry, and there is exactly one of
// it. The shipped catalogue below and [compute.command_risks] in an
// operator's config unmarshal into this same type through this same
// code, so a deployment can express everything the shipped file can.
type Rule = config.CommandRiskConfig

// The catalogue is DATA, and the same data an operator writes.
//
// It used to be a Go map, and the config that extended it was a
// separate struct hand-maintained to mirror it. They drifted: the Go
// rule grew FlagSub and the config one did not, so a deployment could
// not add a flag-driven tool at all. Two mirrors of one concept
// eventually disagree, and the disagreement is silent.
//
// One grammar now. This file and [compute.command_risks] unmarshal
// into the same type through the same code, so that gap cannot reopen.

//go:embed commands.toml
var embeddedTable []byte

// tableFile is the on-disk shape: named rules that entries may extend,
// and the entries themselves.
type tableFile struct {
	Rules    map[string]Rule `toml:"rules"`
	Commands map[string]Rule `toml:"commands"`
}

// DefaultCommandRisks is the shipped catalogue, parsed at init.
var DefaultCommandRisks = mustLoadEmbedded()

// mustLoadEmbedded parses the embedded table, and panics if it cannot.
//
// A panic rather than a fallback, deliberately. The alternative is a
// node that boots with an empty or partial catalogue, which does not
// fail — it classifies every command as unreadable and asks about all
// of them, which reads exactly like the gate working. A build that
// cannot read its own table is broken, and should say so at the
// loudest moment available.
func mustLoadEmbedded() map[string]CommandRiskRule {
	table, err := ParseTable(embeddedTable)
	if err != nil {
		panic("commandrisk: the embedded command table is unusable: " + err.Error())
	}
	return table
}

// RuleFromConfig converts one operator-authored entry, resolving
// extends against the shipped catalogue that config merges over.
//
// The same converter the embedded file uses. It used to be sixty lines
// of near-identical code in the node wiring, which is how config came
// to be missing flag_subcommands, operand_labels and target_last: two
// implementations of one grammar, and only one of them grew.
func RuleFromConfig(name string, r Rule, peers map[string]Rule) (CommandRiskRule, error) {
	shipped := tableFile{Commands: map[string]Rule{}, Rules: peers}
	for n, v := range peers {
		shipped.Commands[n] = v
	}
	// Extends may also name a shipped entry, which is already resolved.
	if r.Extends != "" {
		if _, ok := shipped.Commands[r.Extends]; !ok {
			if base, ok := DefaultCommandRisks[r.Extends]; ok {
				own, err := convert(r)
				if err != nil {
					return CommandRiskRule{}, err
				}
				return inherit(base, own, r), nil
			}
		}
	}
	return resolve(name, r, shipped, 0)
}

// ParseTable reads a TOML catalogue, resolving extends and checking
// every label against the closed set.
//
// An unknown label is an ERROR rather than a dropped field. A typo that
// quietly removed a "deletes" would leave the command classified as
// whatever remained, and that is the one failure this whole subsystem
// exists to prevent.
func ParseTable(data []byte) (map[string]CommandRiskRule, error) {
	var f tableFile
	if err := toml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	resolved := make(map[string]CommandRiskRule, len(f.Commands))
	names := make([]string, 0, len(f.Commands))
	for name := range f.Commands {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		rule, err := resolve(name, f.Commands[name], f, 0)
		if err != nil {
			return nil, fmt.Errorf("command %q: %w", name, err)
		}
		resolved[name] = rule
	}
	return resolved, nil
}

// resolve applies inheritance then converts to the runtime rule.
//
// Depth-limited rather than cycle-detected: a chain longer than this is
// nobody's intent, and a bound is a shorter thing to be sure of than a
// visited-set.
func resolve(name string, t Rule, f tableFile, depth int) (CommandRiskRule, error) {
	if depth > 4 {
		return CommandRiskRule{}, fmt.Errorf("extends chain too deep")
	}
	if t.Extends != "" {
		parentSrc, ok := f.Commands[t.Extends]
		if !ok {
			if parentSrc, ok = f.Rules[t.Extends]; !ok {
				return CommandRiskRule{}, fmt.Errorf("extends %q, which is not defined", t.Extends)
			}
		}
		parent, err := resolve(t.Extends, parentSrc, f, depth+1)
		if err != nil {
			return CommandRiskRule{}, fmt.Errorf("extends %q: %w", t.Extends, err)
		}
		own, err := convert(t)
		if err != nil {
			return CommandRiskRule{}, err
		}
		return inherit(parent, own, t), nil
	}
	return convert(t)
}

// inherit lays a child's own fields over its parent's, key by key for
// the maps so a child can correct one verb without restating the rest.
func inherit(parent, own CommandRiskRule, src Rule) CommandRiskRule {
	out := parent
	if len(own.Labels) > 0 {
		out.Labels = own.Labels
	}
	if len(own.OperandLabels) > 0 {
		out.OperandLabels = own.OperandLabels
	}
	if len(own.ScratchLabels) > 0 {
		out.ScratchLabels = own.ScratchLabels
	}
	if own.Why != "" {
		out.Why = own.Why
	}
	if src.Targets {
		out.Targets = true
	}
	if src.TargetLast {
		out.TargetLast = true
	}
	out.Sub = overlay(parent.Sub, own.Sub)
	out.FlagSub = overlay(parent.FlagSub, own.FlagSub)
	out.Escalate = overlay(parent.Escalate, own.Escalate)
	return out
}

func overlay(parent, child map[string][]RiskLabel) map[string][]RiskLabel {
	if len(parent) == 0 && len(child) == 0 {
		return nil
	}
	out := make(map[string][]RiskLabel, len(parent)+len(child))
	for k, v := range parent {
		out[k] = v
	}
	for k, v := range child {
		out[k] = v
	}
	return out
}

// convert turns the file shape into the runtime rule, validating every
// label on the way through.
func convert(t Rule) (CommandRiskRule, error) {
	var out CommandRiskRule
	var err error
	if out.Labels, err = labels(t.Labels); err != nil {
		return out, err
	}
	if out.OperandLabels, err = labels(t.OperandLabels); err != nil {
		return out, fmt.Errorf("operand_labels: %w", err)
	}
	if out.ScratchLabels, err = labels(t.ScratchLabels); err != nil {
		return out, fmt.Errorf("scratch_labels: %w", err)
	}
	if out.Sub, err = labelMap(t.Subcommands); err != nil {
		return out, fmt.Errorf("subcommands: %w", err)
	}
	if out.FlagSub, err = labelMap(t.FlagSubcommands); err != nil {
		return out, fmt.Errorf("flag_subcommands: %w", err)
	}
	if out.Escalate, err = labelMap(t.Escalate); err != nil {
		return out, fmt.Errorf("escalate: %w", err)
	}
	out.Targets, out.TargetLast, out.Why = t.Targets, t.TargetLast, t.Why
	return out, nil
}

func labels(in []string) ([]RiskLabel, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]RiskLabel, 0, len(in))
	for _, raw := range in {
		l := RiskLabel(strings.ToLower(strings.TrimSpace(raw)))
		if !l.Valid() {
			return nil, fmt.Errorf("unknown label %q (want one of %s)", raw, RenderLabels(AllRiskLabels))
		}
		out = append(out, l)
	}
	return out, nil
}

func labelMap(in map[string][]string) (map[string][]RiskLabel, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string][]RiskLabel, len(in))
	for k, v := range in {
		ls, err := labels(v)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", k, err)
		}
		out[k] = ls
	}
	return out, nil
}
