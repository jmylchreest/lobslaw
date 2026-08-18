package main

import (
	"bytes"
	"strings"
	"testing"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// `learned approve` existed and `learned show` did not, so the only
// way to read what you were approving was to stop the node and open
// state.db. Propose mode without a way to read a proposal is auto
// mode with extra steps.

func artefact(id, name, body string) *lobslawv1.SelfTaughtRecord {
	return &lobslawv1.SelfTaughtRecord{
		Id: id, Name: name, Body: body,
		Kind:  lobslawv1.SelfTaughtKind_SELF_TAUGHT_KIND_SKILL,
		State: lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_PROPOSED,
	}
}

func TestAnExactIdIsNotShadowedByAnotherArtefactsName(t *testing.T) {
	t.Parallel()
	records := []*lobslawv1.SelfTaughtRecord{
		artefact("skill:b", "skill:a", "wrong one"),
		artefact("skill:a", "a", "right one"),
	}
	got := findArtefact(records, "skill:a")
	if got == nil || got.GetBody() != "right one" {
		t.Errorf("id lookup matched %v; a name must never shadow an exact id", got)
	}
}

func TestABareNameFindsTheArtefact(t *testing.T) {
	t.Parallel()
	records := []*lobslawv1.SelfTaughtRecord{artefact("skill:Prepare Notes", "Prepare Notes", "body")}
	if got := findArtefact(records, "Prepare Notes"); got == nil {
		t.Error("a bare name found nothing; the listing shows names most prominently")
	}
}

func TestAMissIsNil(t *testing.T) {
	t.Parallel()
	if got := findArtefact([]*lobslawv1.SelfTaughtRecord{artefact("skill:a", "a", "b")}, "ghost"); got != nil {
		t.Errorf("a miss returned %v", got)
	}
}

// The whole body, never elided. A summary is what the listing is for;
// this is the command somebody runs when the summary was not enough,
// and a truncated body would be approved unread just as often as one
// that was never shown.
func TestTheBodyIsPrintedInFull(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("step. ", 400)
	var buf bytes.Buffer
	renderArtefact(&buf, artefact("skill:a", "a", body))
	if !strings.Contains(buf.String(), strings.TrimSpace(body)) {
		t.Errorf("the body was altered or truncated; rendered %d chars of a %d-char body",
			buf.Len(), len(body))
	}
}

// A pending refinement is what `accept` applies, and it is NOT the
// body above it — the live artefact keeps working while the
// refinement waits. Showing one without the other has somebody accept
// a change they never read.
func TestAPendingRefinementIsShownAndSaidToBeWhatAcceptApplies(t *testing.T) {
	t.Parallel()
	rec := artefact("skill:a", "a", "the live body")
	rec.Pending = &lobslawv1.PendingRevision{
		Body:      "the proposed body",
		Rationale: "clearer wording",
		TurnId:    "turn-9",
	}
	var buf bytes.Buffer
	renderArtefact(&buf, rec)
	out := buf.String()

	for _, want := range []string{"the live body", "the proposed body", "clearer wording", "turn-9"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q from the render", want)
		}
	}
	if !strings.Contains(out, "accept applies THIS") {
		t.Error("nothing says which of the two bodies accept would apply")
	}
}

// Files are part of the artefact and part of what approving it
// accepts — a skill whose references never get read is a skill
// approved on its summary.
func TestBundledFilesAreShown(t *testing.T) {
	t.Parallel()
	rec := artefact("skill:a", "a", "body")
	rec.Files = map[string]string{"references/notes.md": "the reference content"}
	var buf bytes.Buffer
	renderArtefact(&buf, rec)
	out := buf.String()
	if !strings.Contains(out, "references/notes.md") || !strings.Contains(out, "the reference content") {
		t.Errorf("bundled files missing from the render:\n%s", out)
	}
}

// show must reach a live implementation. `nodeid` was documented,
// dispatched by nothing, and booted a whole node.
func TestLearnedShowIsRoutedAndLive(t *testing.T) {
	t.Parallel()
	if _, ok := learnedLiveOnly["show"]; !ok {
		t.Fatal("learned show is not in the routing table")
	}
	if !strings.Contains(learnedUsage, "show") {
		t.Error("learned show is routed but not documented in the usage")
	}
}
