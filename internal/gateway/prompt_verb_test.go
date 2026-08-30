package gateway

import (
	"errors"
	"strings"
	"testing"
)

func recordingGrants(session, always string, noun string) (grantFns, *[]string) {
	called := &[]string{}
	return grantFns{
		session: func() string { *called = append(*called, "session"); return session },
		always:  func() string { *called = append(*called, "always"); return always },
		noun:    noun,
	}, called
}

// The property this extraction exists for: a grant that could not be
// recorded must narrow to a one-shot AND must not tell the user the
// agent will stop asking. Getting only half of that right is the
// failure — the scope is what the code does, the reply is what the
// user believes.
func TestAFailedGrantNarrowsAndSaysNothingMore(t *testing.T) {
	t.Parallel()
	for _, verb := range []string{"approve-session", "approve-always"} {
		grants, _ := recordingGrants("", "", "conversation")
		out, ok := resolvePromptVerb(verb, grants)
		if !ok {
			t.Fatalf("%s: not recognised", verb)
		}
		if out.Scope != PromptScopeOnce {
			t.Errorf("%s: Scope = %v, want PromptScopeOnce when nothing was recorded", verb, out.Scope)
		}
		if out.Decision != PromptApproved {
			t.Errorf("%s: a failed grant must still approve THIS call", verb)
		}
		if out.Reply != "Approved." {
			t.Errorf("%s: Reply = %q — it must not promise a grant that was not recorded", verb, out.Reply)
		}
	}
}

// A recorded grant keeps its scope and says what it covers.
func TestARecordedGrantKeepsItsScope(t *testing.T) {
	t.Parallel()
	grants, _ := recordingGrants("shell:run git status", "shell:run git status", "conversation")

	sess, _ := resolvePromptVerb("approve-session", grants)
	if sess.Scope != PromptScopeSession {
		t.Errorf("Scope = %v, want PromptScopeSession", sess.Scope)
	}
	if !strings.Contains(sess.Reply, "git status") {
		t.Errorf("the reply should name what was granted: %q", sess.Reply)
	}

	always, _ := resolvePromptVerb("approve-always", grants)
	if always.Scope != PromptScopeAlways {
		t.Errorf("Scope = %v, want PromptScopeAlways", always.Scope)
	}
}

// The noun is the one part that SHOULD differ per channel — it is the
// word the user already uses for where they are. Extracting the logic
// must not have flattened it.
func TestTheChannelKeepsItsOwnNoun(t *testing.T) {
	t.Parallel()
	slack, _ := recordingGrants("x", "x", "conversation")
	tg, _ := recordingGrants("x", "x", "chat")

	s, _ := resolvePromptVerb("approve-session", slack)
	g, _ := resolvePromptVerb("approve-session", tg)

	if !strings.Contains(s.Reply, "conversation") {
		t.Errorf("slack reply lost its noun: %q", s.Reply)
	}
	if !strings.Contains(g.Reply, "chat") {
		t.Errorf("telegram reply lost its noun: %q", g.Reply)
	}
}

// A plain approve or deny must not reach a grant path at all.
func TestOnceAndDenyRecordNoGrant(t *testing.T) {
	t.Parallel()
	for verb, want := range map[string]PromptDecision{
		"approve": PromptApproved,
		"deny":    PromptDenied,
	} {
		grants, called := recordingGrants("x", "x", "conversation")
		out, ok := resolvePromptVerb(verb, grants)
		if !ok || out.Decision != want || out.Scope != PromptScopeOnce {
			t.Errorf("%s: got %+v ok=%v", verb, out, ok)
		}
		if len(*called) != 0 {
			t.Errorf("%s reached a grant path: %v", verb, *called)
		}
	}
}

// An unknown verb is malformed callback input. Silence, not an error.
func TestAnUnknownVerbIsRefused(t *testing.T) {
	t.Parallel()
	grants, called := recordingGrants("x", "x", "conversation")
	if _, ok := resolvePromptVerb("approve-everything-forever", grants); ok {
		t.Error("an unrecognised verb was accepted")
	}
	if len(*called) != 0 {
		t.Errorf("an unrecognised verb reached a grant path: %v", *called)
	}
}

// The three Resolve failures stay distinguishable: the user's next
// move differs, and collapsing them makes all three look like a bug.
func TestResolveFailureRepliesStayDistinct(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, err := range []error{ErrPromptNotFound, ErrPromptResolved, errors.New("raft down")} {
		r := resolveFailureReply(err)
		if r == "" {
			t.Fatalf("%v produced an empty reply", err)
		}
		if seen[r] {
			t.Errorf("%v shares a reply with another cause: %q", err, r)
		}
		seen[r] = true
	}
}
