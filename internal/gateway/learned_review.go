package gateway

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/console"
)

var ErrLearnedReviewForbidden = console.ErrLearnedReviewForbidden
var ErrLearnedReviewNotFound = console.ErrLearnedReviewNotFound
var ErrLearnedReviewConflict = console.ErrLearnedReviewConflict

type LearnedReviews = console.LearnedReviews
type LearnedChange = console.LearnedChange
type LearnedReview = console.LearnedReview

const learnedReviewAction = "learned:review"

// Full current and proposed content, including removed files, remains visible.
// Proposal text is data sent directly to the user, never instructions fed to a model.
func renderLearnedReview(r LearnedReview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — revision %d\nID: %s\nSource turn: %s\n", r.Name, r.Revision, r.ID, r.TurnID)
	if r.Pending != nil {
		if r.Active {
			b.WriteString("\nCurrent approved version\n")
		} else {
			b.WriteString("\nOriginal proposal (not active)\n")
		}
		renderLearnedContent(&b, r.Description, r.Body, r.Files)
		fmt.Fprintf(&b, "\nProposed amendment\nWhy: %s\nSource turn: %s\n", r.Pending.Rationale, r.Pending.TurnID)
		renderLearnedContent(&b, r.Pending.Description, r.Pending.Body, r.Pending.Files)
	} else {
		b.WriteString("\nProposed skill (not active)\n")
		renderLearnedContent(&b, r.Description, r.Body, r.Files)
	}
	return b.String()
}

func renderLearnedContent(b *strings.Builder, description, body string, files map[string]string) {
	fmt.Fprintf(b, "%s\n\nInstructions:\n%s\n", description, body)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(b, "\nFile: %s\n%s\n", name, files[name])
	}
}
