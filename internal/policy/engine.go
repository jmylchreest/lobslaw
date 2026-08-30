package policy

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"

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
	store  *memory.Store
	logger *slog.Logger
	// evalMu guards evaluators — RegisterCondition can be called after
	// NewEngine (e.g. when new condition types register lazily at
	// plugin-load), and Evaluate reads evaluators concurrently from
	// any goroutine that processes a tool invocation.
	evalMu     sync.RWMutex
	evaluators map[string]ConditionEvaluator

	// defaults are config-derived rules held in memory, evaluated
	// AFTER everything in the store.
	//
	// Not written to the rule bucket, deliberately. That bucket is
	// raft-replicated operator intent; a rule derived from one node's
	// config file is neither, and every node writing its own copy at
	// boot would turn a local setting into contested cluster state.
	//
	// Last, so anything an operator wrote — and anything an earlier
	// approval minted — outranks them. A default that could not be
	// overridden would not be a default.
	defaultMu sync.RWMutex
	defaults  []types.PolicyRule
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
// Safe to call concurrently with Evaluate.
func (e *Engine) RegisterCondition(key string, fn ConditionEvaluator) {
	e.evalMu.Lock()
	defer e.evalMu.Unlock()
	e.evaluators[key] = fn
}

// lookupEvaluator is the read-guarded counterpart. Returns the
// evaluator or (nil, false) if not registered. Holding the mutex
// while calling the evaluator would serialise every policy check,
// so callers copy the function value out first.
func (e *Engine) lookupEvaluator(key string) (ConditionEvaluator, bool) {
	e.evalMu.RLock()
	defer e.evalMu.RUnlock()
	fn, ok := e.evaluators[key]
	return fn, ok
}

// Evaluate runs the policy decision for (action, resource) in the
// context of claims. Rules are walked in descending priority order;
// the first matching rule's effect wins. Default is deny — if no
// rule matches, Decision.Effect is EffectDeny with an empty RuleID.
//
// A rule whose conditions cannot be evaluated is skipped only if it
// would have allowed; deny and require_confirmation apply anyway.
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
			// An error means we do not know whether this rule applies —
			// its condition referenced an evaluator that isn't
			// registered, or one that failed. That is different from a
			// condition that evaluated cleanly to false, which is a
			// definite "this rule does not apply" and skips below.
			//
			// Skipping on error is only safe for a rule that would have
			// granted something. An allow we drop costs access; a deny
			// we drop costs exactly the protection the rule exists to
			// provide, and evaluation then continues to lower-priority
			// rules — so a broken condition on a high-priority deny
			// hands the request to whatever allow sits underneath it.
			// Restrictive effects therefore apply on error.
			e.logger.Warn("policy: condition evaluation error",
				"rule_id", rule.ID, "effect", rule.Effect, "err", err)
			if rule.Effect == types.EffectAllow {
				continue
			}
			return Decision{
				Effect: rule.Effect,
				RuleID: rule.ID,
				Reason: fmt.Sprintf("rule %q applied without evaluating its conditions (%v)", rule.ID, err),
			}, nil
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

// SetDefaults installs config-derived fallback rules.
//
// Replaces rather than appends, so a reload cannot accumulate
// duplicates of the same setting.
func (e *Engine) SetDefaults(rules []types.PolicyRule) {
	e.defaultMu.Lock()
	defer e.defaultMu.Unlock()
	e.defaults = append([]types.PolicyRule(nil), rules...)
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

	// Appended after the sort rather than sorted in with everything
	// else. Sorting them together would let a default with an
	// unluckily high priority beat an operator rule, and the whole
	// point of a default is that it is what applies when nobody said
	// otherwise.
	e.defaultMu.RLock()
	rules = append(rules, e.defaults...)
	e.defaultMu.RUnlock()
	return rules, nil
}

// conditionsHold returns true when ALL conditions are satisfied.
//
// An unregistered condition key is an error, not a false — the caller
// distinguishes the two. "I evaluated this and it does not hold" lets
// a rule be skipped safely; "I could not evaluate this" must not,
// because a rule carrying a condition nobody understands is exactly
// how an attacker would disarm a deny. See Evaluate.
func (e *Engine) conditionsHold(ctx context.Context, conds []types.Condition) (bool, error) {
	for _, c := range conds {
		fn, ok := e.lookupEvaluator(c.Key)
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
		return slices.Contains(claims.Roles, value)
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
	// Split on the wildcards and walk the literal segments in order.
	//
	// A strict superset of what this used to do, which special-cased
	// only a leading and a trailing star: "foo*" was HasPrefix, "*foo"
	// HasSuffix, "*foo*" Contains, and a star anywhere else was matched
	// as the literal character. Every one of those shapes still gives
	// the same answer here; "a*b" is the case that changes, and it used
	// to be a rule that silently matched nothing.
	//
	// Deliberately not filepath.Match. Its star stops at a separator,
	// so a shipped rule like write_file:/etc/* would quietly narrow
	// from "anything under /etc" to "one level under /etc" — tightening
	// an operator's rule without telling them. Resources here are
	// commands and grant keys, not paths; a separator has no meaning.
	segments := strings.Split(pattern, "*")
	if len(segments) == 1 {
		return pattern == value // no wildcard at all
	}
	// Anchored at the front unless the pattern opens with a star.
	if head := segments[0]; head != "" {
		if !strings.HasPrefix(value, head) {
			return false
		}
		value = value[len(head):]
	}
	// Anchored at the back unless it closes with one. Length-checked
	// first: the head may already have eaten the characters the tail
	// wants, and "ab*ab" must not match "ab" by counting them twice.
	if tail := segments[len(segments)-1]; tail != "" {
		if len(value) < len(tail) || !strings.HasSuffix(value, tail) {
			return false
		}
		value = value[:len(value)-len(tail)]
	}
	// The rest must appear in order, each after the last.
	for _, seg := range segments[1 : len(segments)-1] {
		if seg == "" {
			continue // "**" is just "*"
		}
		i := strings.Index(value, seg)
		if i < 0 {
			return false
		}
		value = value[i+len(seg):]
	}
	return true
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
