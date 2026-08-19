package compute

import "testing"

// The bias is deliberate and matches the prompt's own instruction:
// folding two related messages costs nothing, splitting one thought
// answers half of it. So anything not recognisably NEW is SAME.
func TestOnlyAClearNewSplitsTheTurn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		content string
		related bool
	}{
		{"SAME", true},
		{"NEW", false},
		{"new", false},
		{" NEW ", false},
		{"NEW.", false},
		{"**NEW**", false},
		// Unparseable, empty, or chatty: fold. A model having a bad
		// day must not be able to split a question.
		{"", true},
		{"I think these are related", true},
		{"unclear", true},
	} {
		if got := parseRelatedness(tc.content); got != tc.related {
			t.Errorf("parseRelatedness(%q) = %v, want %v", tc.content, got, tc.related)
		}
	}
}

// A SUBSTRING SEARCH GETS THESE BACKWARDS. Both contain "NEW" and
// both mean the opposite. Unlikely from an instructed model with a
// four-token cap, but a misclassification silently splits a question
// and nothing downstream can tell it happened.
func TestANegatedNewIsNotANew(t *testing.T) {
	t.Parallel()
	for _, content := range []string{"not NEW", "NOT NEW", "Same, not NEW"} {
		if !parseRelatedness(content) {
			t.Errorf("parseRelatedness(%q) split the turn on a negation", content)
		}
	}
}
