package policy

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Decision is the result of evaluating a request against the rules.
type Decision struct {
	Effect types.Effect // allow | deny | require_confirmation
	RuleID string       // matched rule id; empty when default-deny
	Reason string       // human-readable
}

// ConditionEvaluator checks a typed Condition. Rule evaluation skips
// any rule whose conditions include a type not registered here
// (fail-closed — unknown conditions never cause allow).
//
// Implementations register themselves via Engine.RegisterCondition.
// Known types in MVP: empty (none registered by default). Phase 4.5
// will add time_of_day and peer_cidr evaluators wired against clock
// + gRPC peer info.
type ConditionEvaluator func(ctx context.Context, cond types.Condition) (bool, error)

// Engine evaluates policy requests against the Raft-replicated rule
// store. Reads hit the local store (no Raft round-trip), so an Engine
// is cheap to construct and safe to share across goroutines.
type Engine struct {
	store      *memory.Store
	logger     *slog.Logger
	evaluators map[string]ConditionEvaluator
}

// NewEngine wraps store with an Engine. The store must be the same
// one driving the FSM so rule writes through raft.Apply are visible.
func NewEngine(store *memory.Store, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{
		store:      store,
		logger:     logger,
		evaluators: make(map[string]ConditionEvaluator),
	}
}

// RegisterCondition installs an evaluator for a condition type.
// Overwrites any previously-registered evaluator for the same key.
func (e *Engine) RegisterCondition(key string, fn ConditionEvaluator) {
	e.evaluators[key] = fn
}

// Evaluate runs the policy decision for (action, resource) in the
// context of claims. Rules are walked in descending priority order;
// the first matching rule's effect wins. Default is deny — if no
// rule matches, Decision.Effect is EffectDeny with an empty RuleID.
func (e *Engine) Evaluate(ctx context.Context, claims *types.Claims, action, resource string) (Decision, error) {
	if claims == nil {
		return Decision{
			Effect: types.EffectDeny,
			Reason: "no claims",
		}, nil
	}
	rules, err := e.loadRules()
	if err != nil {
		return Decision{}, fmt.Errorf("load rules: %w", err)
	}

	for _, rule := range rules {
		if !subjectMatches(rule.Subject, claims) {
			continue
		}
		if !patternMatches(rule.Action, action) {
			continue
		}
		if !patternMatches(rule.Resource, resource) {
			continue
		}
		if rule.Scope != "" && rule.Scope != claims.Scope {
			continue
		}
		if ok, err := e.conditionsHold(ctx, rule.Conditions); err != nil {
			e.logger.Warn("policy: condition evaluation error",
				"rule_id", rule.ID, "err", err)
			continue
		} else if !ok {
			continue
		}

		return Decision{
			Effect: rule.Effect,
			RuleID: rule.ID,
			Reason: fmt.Sprintf("rule %q matched (%s/%s)", rule.ID, rule.Action, rule.Resource),
		}, nil
	}

	return Decision{
		Effect: types.EffectDeny,
		Reason: "no rule matched (default-deny)",
	}, nil
}

// loadRules reads all PolicyRule records from the store and sorts
// them by priority descending. Fresh read on each Evaluate call —
// rules change rarely and this keeps the path simple. A cache layer
// with FSM-driven invalidation can land later if measurement warrants.
func (e *Engine) loadRules() ([]types.PolicyRule, error) {
	var rules []types.PolicyRule
	err := e.store.ForEach(memory.BucketPolicyRules, func(_ string, raw []byte) error {
		var p lobslawv1.PolicyRule
		if err := proto.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("unmarshal policy rule: %w", err)
		}
		rules = append(rules, protoToRule(&p))
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Higher priority first; ties broken by rule ID for determinism.
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].Priority != rules[j].Priority {
			return rules[i].Priority > rules[j].Priority
		}
		return rules[i].ID < rules[j].ID
	})
	return rules, nil
}

// conditionsHold returns true when ALL conditions are satisfied.
// Unknown condition types fail the entire rule (unknown = not-matched),
// so an attacker who adds a rule with a novel condition can't slip
// past the check.
func (e *Engine) conditionsHold(ctx context.Context, conds []types.Condition) (bool, error) {
	for _, c := range conds {
		fn, ok := e.evaluators[c.Key]
		if !ok {
			return false, fmt.Errorf("no evaluator for condition key %q", c.Key)
		}
		hold, err := fn(ctx, c)
		if err != nil {
			return false, err
		}
		if !hold {
			return false, nil
		}
	}
	return true, nil
}

// subjectMatches compares a rule subject like "user:alice",
// "role:admin", "scope:default", or "*" against the claims.
func subjectMatches(subject string, claims *types.Claims) bool {
	if subject == "" || subject == "*" {
		return true
	}
	kind, value, ok := strings.Cut(subject, ":")
	if !ok {
		// Malformed subject — treat as no-match (fail closed).
		return false
	}
	switch kind {
	case "user":
		return claims.UserID == value
	case "role":
		for _, r := range claims.Roles {
			if r == value {
				return true
			}
		}
		return false
	case "scope":
		return claims.Scope == value
	default:
		// Unknown subject kind — fail closed.
		return false
	}
}

// patternMatches supports exact match and simple glob shapes:
//
//	"exact"     exact equality
//	"prefix*"   value starts with prefix
//	"*suffix"   value ends with suffix
//	"*mid*"     value contains mid
//	"*"         matches anything
//
// Same semantics as the log filter library — consistent operator
// mental model across policy and logging.
func patternMatches(pattern, value string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	starPrefix := strings.HasPrefix(pattern, "*")
	starSuffix := strings.HasSuffix(pattern, "*")
	switch {
	case starPrefix && starSuffix:
		mid := strings.TrimPrefix(strings.TrimSuffix(pattern, "*"), "*")
		return strings.Contains(value, mid)
	case starPrefix:
		return strings.HasSuffix(value, strings.TrimPrefix(pattern, "*"))
	case starSuffix:
		return strings.HasPrefix(value, strings.TrimSuffix(pattern, "*"))
	default:
		return pattern == value
	}
}

// protoToRule converts the proto-wire PolicyRule into the typed
// internal form. Effect is passed through verbatim — if a rule carries
// an unknown effect string, IsValid() would catch it at write time.
func protoToRule(p *lobslawv1.PolicyRule) types.PolicyRule {
	conds := make([]types.Condition, 0, len(p.Conditions))
	for _, c := range p.Conditions {
		conds = append(conds, types.Condition{Key: c.Key, Op: c.Op, Value: c.Value})
	}
	return types.PolicyRule{
		ID:         p.Id,
		Subject:    p.Subject,
		Action:     p.Action,
		Resource:   p.Resource,
		Effect:     types.Effect(p.Effect),
		Conditions: conds,
		Priority:   int(p.Priority),
		Scope:      p.Scope,
	}
}
