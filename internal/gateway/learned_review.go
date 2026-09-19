package gateway

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// LearnedReviews is the human review surface. It is deliberately separate
// from agent tools. Implementations authorise every read and decision.
type LearnedReviews interface {
	List(context.Context, *types.Claims) ([]LearnedReview, error)
	Get(context.Context, *types.Claims, string) (LearnedReview, error)
	Decide(context.Context, *types.Claims, string, uint64, string, bool) (string, error)
}

type LearnedChange struct {
	Description string
	Body        string
	Files       map[string]string
	Rationale   string
	TurnID      string
}

type LearnedReview struct {
	ID          string
	Name        string
	Description string
	Body        string
	Files       map[string]string
	Revision    uint64
	Digest      string
	TurnID      string
	Active      bool
	Pending     *LearnedChange
}

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
