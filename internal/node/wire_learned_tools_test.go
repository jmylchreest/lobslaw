package node

import (
	"testing"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// learned_list must register AFTER the stage that sets n.selfTaught.
// Registering before it finds nil and skips the tool on every node
// that has the store — silently, because absence is how this codebase
// expresses "not configured".
func TestTheSelfTaughtStageRunsBeforeCompute(t *testing.T) {
	t.Parallel()
	selfTaught, compute := -1, -1
	for i, st := range nodeWireStages() {
		switch st.Name {
		case "self-taught":
			selfTaught = i
		case "compute":
			compute = i
		}
	}
	if selfTaught < 0 || compute < 0 {
		t.Fatalf("stages missing: self-taught=%d compute=%d", selfTaught, compute)
	}
	if selfTaught > compute {
		t.Errorf("self-taught runs at %d, after compute at %d; learned_list would never register",
			selfTaught, compute)
	}
}

// The words the agent says must be the words the operator types, or
// "proposed" in a chat reply does not match `lobslaw learned list`.
func TestTheEnumLabelsMatchTheCLIVocabulary(t *testing.T) {
	t.Parallel()
	if got := stateName(lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_PROPOSED); got != "proposed" {
		t.Errorf("state = %q, want proposed", got)
	}
	if got := stateName(lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_ACTIVE); got != "active" {
		t.Errorf("state = %q, want active", got)
	}
	if got := kindName(lobslawv1.SelfTaughtKind_SELF_TAUGHT_KIND_SKILL); got != "skill" {
		t.Errorf("kind = %q, want skill", got)
	}
	// Derived from the enum name, so a kind added later renders as
	// itself rather than as "unknown" — which is what the first
	// hand-written switch here did.
	if got := kindName(lobslawv1.SelfTaughtKind_SELF_TAUGHT_KIND_PROCEDURE); got != "procedure" {
		t.Errorf("a later-added kind rendered as %q", got)
	}
}
