package gateway

import (
	"strings"
	"testing"
)

func TestLearnedReviewIncludesWholeProposal(t *testing.T) {
	t.Parallel()
	r := LearnedReview{ID: "skill:tidy", Name: "tidy", Revision: 3, Body: "old instructions", Files: map[string]string{"old.txt": "old reference"}, Pending: &LearnedChange{Body: "new instructions", Description: "better", Rationale: "user correction", Files: map[string]string{"new.txt": "new reference"}}}
	text := renderLearnedReview(r)
	for _, want := range []string{"old instructions", "new instructions", "old reference", "new reference", "user correction", "revision 3"} {
		if !strings.Contains(text, want) {
			t.Errorf("review missing %q: %s", want, text)
		}
	}
}
