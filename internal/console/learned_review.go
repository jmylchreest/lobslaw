package console

import (
	"context"
	"errors"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

var (
	ErrLearnedReviewForbidden = errors.New("not authorised to review learned skills")
	ErrLearnedReviewNotFound  = errors.New("proposal not found for this user")
	ErrLearnedReviewConflict  = errors.New("proposal changed or already decided; reload before reviewing again")
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
	Author      string
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
