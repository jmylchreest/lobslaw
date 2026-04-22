package policy

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func newTestEngine(t *testing.T) (*Engine, *memory.Store) {
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
	return NewEngine(store, nil), store
}

// seedRule writes a rule directly to the store (bypassing Raft —
// these are unit tests for the engine, not the service).
func seedRule(t *testing.T, store *memory.Store, r *lobslawv1.PolicyRule) {
	t.Helper()
	raw, err := proto.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(memory.BucketPolicyRules, r.Id, raw); err != nil {
		t.Fatal(err)
	}
}

func TestEngineDefaultDeny(t *testing.T) {
	t.Parallel()
	eng, _ := newTestEngine(t)
	dec, err := eng.Evaluate(context.Background(),
		&types.Claims{UserID: "alice"}, "tool:exec", "bash")
	if err != nil {
		t.Fatal(err)
	}
	if dec.Effect != types.EffectDeny {
		t.Errorf("effect = %v, want deny", dec.Effect)
	}
	if dec.RuleID != "" {
		t.Errorf("RuleID = %q, want empty (no rule matched)", dec.RuleID)
	}
}

func TestEngineMatchesExact(t *testing.T) {
	t.Parallel()
	eng, store := newTestEngine(t)
	seedRule(t, store, &lobslawv1.PolicyRule{
		Id:       "r-1",
		Subject:  "user:alice",
		Action:   "tool:exec",
		Resource: "bash",
		Effect:   "allow",
		Priority: 10,
	})
	dec, err := eng.Evaluate(context.Background(),
		&types.Claims{UserID: "alice"}, "tool:exec", "bash")
	if err != nil {
		t.Fatal(err)
	}
	if dec.Effect != types.EffectAllow {
		t.Errorf("effect = %v, want allow", dec.Effect)
	}
	if dec.RuleID != "r-1" {
		t.Errorf("RuleID = %q, want r-1", dec.RuleID)
	}
}

func TestEnginePriorityOrderingWins(t *testing.T) {
	t.Parallel()
	eng, store := newTestEngine(t)
	// Lower priority deny, higher priority allow. Higher should win.
	seedRule(t, store, &lobslawv1.PolicyRule{
		Id: "low", Subject: "user:alice", Action: "tool:exec", Resource: "*",
		Effect: "deny", Priority: 1,
	})
	seedRule(t, store, &lobslawv1.PolicyRule{
		Id: "high", Subject: "user:alice", Action: "tool:exec", Resource: "bash",
		Effect: "allow", Priority: 100,
	})
	dec, err := eng.Evaluate(context.Background(),
		&types.Claims{UserID: "alice"}, "tool:exec", "bash")
	if err != nil {
		t.Fatal(err)
	}
	if dec.RuleID != "high" {
		t.Errorf("RuleID = %q, want high (priority ordering broken)", dec.RuleID)
	}
}

func TestEnginePriorityTieBreaksByID(t *testing.T) {
	t.Parallel()
	eng, store := newTestEngine(t)
	seedRule(t, store, &lobslawv1.PolicyRule{
		Id: "b", Subject: "*", Action: "*", Resource: "*",
		Effect: "allow", Priority: 5,
	})
	seedRule(t, store, &lobslawv1.PolicyRule{
		Id: "a", Subject: "*", Action: "*", Resource: "*",
		Effect: "deny", Priority: 5,
	})
	dec, err := eng.Evaluate(context.Background(),
		&types.Claims{UserID: "x"}, "anything", "anything")
	if err != nil {
		t.Fatal(err)
	}
	// Ties broken by ID ascending — "a" beats "b" at equal priority.
	if dec.RuleID != "a" {
		t.Errorf("RuleID = %q, want a (tie-break by id)", dec.RuleID)
	}
}

func TestEngineRoleSubject(t *testing.T) {
	t.Parallel()
	eng, store := newTestEngine(t)
	seedRule(t, store, &lobslawv1.PolicyRule{
		Id: "admins-ok", Subject: "role:admin", Action: "*", Resource: "*",
		Effect: "allow", Priority: 10,
	})
	// Alice has role; Bob doesn't.
	aliceDec, _ := eng.Evaluate(context.Background(),
		&types.Claims{UserID: "alice", Roles: []string{"admin", "user"}},
		"any", "any")
	bobDec, _ := eng.Evaluate(context.Background(),
		&types.Claims{UserID: "bob", Roles: []string{"user"}},
		"any", "any")
	if aliceDec.Effect != types.EffectAllow {
		t.Errorf("alice: %v, want allow", aliceDec.Effect)
	}
	if bobDec.Effect != types.EffectDeny {
		t.Errorf("bob: %v, want deny (no admin role)", bobDec.Effect)
	}
}

func TestEngineScopeFilter(t *testing.T) {
	t.Parallel()
	eng, store := newTestEngine(t)
	seedRule(t, store, &lobslawv1.PolicyRule{
		Id: "work-only", Subject: "*", Action: "*", Resource: "*",
		Effect: "allow", Priority: 10, Scope: "work",
	})
	workDec, _ := eng.Evaluate(context.Background(),
		&types.Claims{Scope: "work"}, "a", "b")
	homeDec, _ := eng.Evaluate(context.Background(),
		&types.Claims{Scope: "home"}, "a", "b")
	if workDec.Effect != types.EffectAllow {
		t.Errorf("work scope: %v, want allow", workDec.Effect)
	}
	if homeDec.Effect != types.EffectDeny {
		t.Errorf("home scope: %v, want deny", homeDec.Effect)
	}
}

func TestEngineRequireConfirmationEffect(t *testing.T) {
	t.Parallel()
	eng, store := newTestEngine(t)
	seedRule(t, store, &lobslawv1.PolicyRule{
		Id: "irreversible", Subject: "*", Action: "tool:exec", Resource: "rm",
		Effect: "require_confirmation", Priority: 10,
	})
	dec, err := eng.Evaluate(context.Background(),
		&types.Claims{UserID: "alice"}, "tool:exec", "rm")
	if err != nil {
		t.Fatal(err)
	}
	if dec.Effect != types.EffectRequireConfirmation {
		t.Errorf("effect = %v, want require_confirmation", dec.Effect)
	}
}

func TestEngineUnknownConditionFailsClosed(t *testing.T) {
	t.Parallel()
	eng, store := newTestEngine(t)
	seedRule(t, store, &lobslawv1.PolicyRule{
		Id: "r", Subject: "*", Action: "*", Resource: "*",
		Effect: "allow", Priority: 10,
		Conditions: []*lobslawv1.Condition{
			{Key: "time_of_day", Op: "between", Value: "09:00-17:00"},
		},
	})
	// No evaluator registered for "time_of_day" → rule should be
	// skipped → default-deny.
	dec, err := eng.Evaluate(context.Background(),
		&types.Claims{UserID: "alice"}, "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if dec.Effect != types.EffectDeny {
		t.Errorf("unknown condition should fail closed: got %v", dec.Effect)
	}
}

func TestEngineConditionEvaluatorTrue(t *testing.T) {
	t.Parallel()
	eng, store := newTestEngine(t)
	eng.RegisterCondition("always", func(_ context.Context, _ types.Condition) (bool, error) {
		return true, nil
	})
	seedRule(t, store, &lobslawv1.PolicyRule{
		Id: "r", Subject: "*", Action: "*", Resource: "*",
		Effect: "allow", Priority: 10,
		Conditions: []*lobslawv1.Condition{{Key: "always"}},
	})
	dec, err := eng.Evaluate(context.Background(),
		&types.Claims{UserID: "alice"}, "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if dec.Effect != types.EffectAllow {
		t.Errorf("effect = %v, want allow (condition held)", dec.Effect)
	}
}

func TestEngineConditionEvaluatorFalseSkipsRule(t *testing.T) {
	t.Parallel()
	eng, store := newTestEngine(t)
	eng.RegisterCondition("never", func(_ context.Context, _ types.Condition) (bool, error) {
		return false, nil
	})
	seedRule(t, store, &lobslawv1.PolicyRule{
		Id: "cond-allow", Subject: "*", Action: "*", Resource: "*",
		Effect: "allow", Priority: 100,
		Conditions: []*lobslawv1.Condition{{Key: "never"}},
	})
	seedRule(t, store, &lobslawv1.PolicyRule{
		Id: "fallback-deny", Subject: "*", Action: "*", Resource: "*",
		Effect: "deny", Priority: 1,
	})
	dec, err := eng.Evaluate(context.Background(),
		&types.Claims{UserID: "alice"}, "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if dec.RuleID != "fallback-deny" {
		t.Errorf("RuleID = %q, want fallback-deny (first rule's condition false)", dec.RuleID)
	}
}

func TestEngineConditionEvaluatorErrorSkipsRule(t *testing.T) {
	t.Parallel()
	eng, store := newTestEngine(t)
	eng.RegisterCondition("broken", func(_ context.Context, _ types.Condition) (bool, error) {
		return false, errors.New("transient failure")
	})
	seedRule(t, store, &lobslawv1.PolicyRule{
		Id: "r", Subject: "*", Action: "*", Resource: "*",
		Effect: "allow", Priority: 10,
		Conditions: []*lobslawv1.Condition{{Key: "broken"}},
	})
	dec, err := eng.Evaluate(context.Background(),
		&types.Claims{UserID: "alice"}, "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if dec.Effect != types.EffectDeny {
		t.Errorf("evaluator error should fail closed: got %v", dec.Effect)
	}
}

func TestEngineNilClaims(t *testing.T) {
	t.Parallel()
	eng, _ := newTestEngine(t)
	dec, err := eng.Evaluate(context.Background(), nil, "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if dec.Effect != types.EffectDeny {
		t.Errorf("nil claims: %v, want deny", dec.Effect)
	}
}

func TestPatternMatches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern, value string
		want           bool
	}{
		{"", "anything", true},
		{"*", "anything", true},
		{"memory:read", "memory:read", true},
		{"memory:read", "memory:write", false},
		{"memory:*", "memory:read", true},
		{"memory:*", "memory:write", true},
		{"memory:*", "tool:exec", false},
		{"*:read", "memory:read", true},
		{"*:read", "tool:exec", false},
		{"*error*", "big_error_here", true},
		{"*error*", "no-match", false},
		{"exact", "not-exact", false},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+"|"+tc.value, func(t *testing.T) {
			if got := patternMatches(tc.pattern, tc.value); got != tc.want {
				t.Errorf("patternMatches(%q, %q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
			}
		})
	}
}

func TestSubjectMatches(t *testing.T) {
	t.Parallel()
	claims := &types.Claims{
		UserID: "alice",
		Roles:  []string{"admin", "user"},
		Scope:  "work",
	}
	cases := []struct {
		subject string
		want    bool
	}{
		{"", true},
		{"*", true},
		{"user:alice", true},
		{"user:bob", false},
		{"role:admin", true},
		{"role:user", true},
		{"role:guest", false},
		{"scope:work", true},
		{"scope:home", false},
		{"node:n-1", false}, // unknown kind — fail closed
		{"bogus", false},    // malformed — fail closed
	}
	for _, tc := range cases {
		t.Run(tc.subject, func(t *testing.T) {
			if got := subjectMatches(tc.subject, claims); got != tc.want {
				t.Errorf("subjectMatches(%q) = %v, want %v", tc.subject, got, tc.want)
			}
		})
	}
}

func TestEngineMalformedRuleDoesntBreakEvaluation(t *testing.T) {
	t.Parallel()
	eng, store := newTestEngine(t)
	// Bogus raw bytes in the bucket — should be surfaced as an error
	// from loadRules.
	if err := store.Put(memory.BucketPolicyRules, "garbage", []byte{0xff, 0xff, 0xff}); err != nil {
		t.Fatal(err)
	}
	_, err := eng.Evaluate(context.Background(),
		&types.Claims{UserID: "alice"}, "a", "b")
	if err == nil {
		t.Fatal("expected error from malformed rule bytes")
	}
}
