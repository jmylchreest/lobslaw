package compute

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type fakeLearned struct {
	items []LearnedSummary
	owner string
}

func (f *fakeLearned) ListForAgent(_ context.Context, owner string) ([]LearnedSummary, error) {
	f.owner = owner
	return f.items, nil
}

// Asked whether it had proposed anything, the assistant reached for
// memory_recent, queried the episodic store, got a truthful empty and
// reported it — while two proposals sat in a bucket it had no way to
// name. "The agent cannot see what it proposed" is defensible; "the
// agent answers no when the answer is two" is not.
func TestTheAgentCanSeeItsOwnProposals(t *testing.T) {
	t.Parallel()
	store := &fakeLearned{items: []LearnedSummary{
		{ID: "skill:Triage Incident", Kind: "skill", Name: "Triage Incident", State: "proposed"},
		{ID: "skill:Prepare Release Notes", Kind: "skill", Name: "Prepare Release Notes", State: "active"},
	}}
	b := NewBuiltins()
	if err := RegisterLearnedBuiltins(b, LearnedConfig{Store: store}); err != nil {
		t.Fatal(err)
	}
	fn, ok := b.Get("learned_list")
	if !ok {
		t.Fatal("learned_list did not register")
	}
	out, code, err := fn(context.Background(), nil)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	var decoded struct {
		Artefacts []LearnedSummary `json:"artefacts"`
		Count     int              `json:"count"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Count != 2 {
		t.Errorf("count = %d, want 2", decoded.Count)
	}
	if decoded.Artefacts[0].State != "proposed" {
		t.Errorf("state = %q; the whole point is knowing what is waiting", decoded.Artefacts[0].State)
	}
}

// A nil store disables the tool rather than registering one that
// answers "not configured" — which reads to a model as a transient
// failure worth retrying.
func TestNoStoreMeansNoTool(t *testing.T) {
	t.Parallel()
	if err := RegisterLearnedBuiltins(NewBuiltins(), LearnedConfig{}); err == nil {
		t.Error("a nil store registered a tool anyway")
	}
}

// THE ONE THAT MATTERS FOR SAFETY. The store is narrow so the review
// fork cannot reach past its own namespace; a tool that approved
// would hand back exactly what that design withholds. Approval is a
// decision the requester must not also be able to make.
func TestThereIsNoWayToApproveFromATool(t *testing.T) {
	t.Parallel()
	for _, td := range LearnedToolDefs() {
		lower := strings.ToLower(td.Name + " " + string(td.ParametersSchema))
		for _, forbidden := range []string{"approve", "accept", "activate", "reject", "archive"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("tool %q exposes %q; approval must stay operator-only", td.Name, forbidden)
			}
		}
		// Reading runs nothing, the same trade skill_view makes.
		if td.RiskTier != types.RiskReversible {
			t.Errorf("tool %q is %v; listing acts on nothing", td.Name, td.RiskTier)
		}
	}
}

// The description has to say these are invisible elsewhere, because
// the failure was the model confidently querying the wrong store.
func TestTheDescriptionSaysWhereTheseDoNotAppear(t *testing.T) {
	t.Parallel()
	d := LearnedToolDefs()[0].Description
	for _, want := range []string{"memory_search", "memory_recent", "skill_view"} {
		if !strings.Contains(d, want) {
			t.Errorf("the description does not warn that %s cannot see these", want)
		}
	}
	if !strings.Contains(d, "learned approve") {
		t.Error("the description does not say who can approve, so the agent cannot say either")
	}
}

// THE SAME BUG ONE LAYER DOWN. The review fork stamps ownership as
// "user:" + UserID. A filter built from the bare principal matches
// nothing, and the tool answers "you have proposed nothing" while the
// proposals sit in the store — which is exactly the failure this
// builtin exists to fix.
func TestTheOwnerFilterMatchesWhatTheReviewForkStamps(t *testing.T) {
	t.Parallel()
	got := learnedOwner(turn.Identity{UserID: "anon"})
	if got != "user:anon" {
		t.Errorf("owner filter = %q, want the form ownerOf writes (%q)", got, "user:anon")
	}
	if got := learnedOwner(turn.Identity{UserID: "tg-@alice"}); got != "user:tg-@alice" {
		t.Errorf("owner filter = %q", got)
	}
}

// An anonymous turn asking what the assistant has learned should see
// the assistant's artefacts, not an empty list. Empty means "do not
// filter", not "match the empty owner".
func TestAnIdentitylessTurnDoesNotFilterToNothing(t *testing.T) {
	t.Parallel()
	if got := learnedOwner(turn.Identity{}); got != "" {
		t.Errorf("owner filter = %q, want empty so the store does not filter", got)
	}
}
