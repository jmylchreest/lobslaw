package console

import (
	"context"
	"errors"
	"time"

	"github.com/jmylchreest/lobslaw/internal/turn"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

var ErrPromptNotFound = turn.ErrPromptNotFound
var ErrPromptExpired = errors.New("prompt: expired")
var ErrPromptResolved = turn.ErrPromptResolved

type PromptView struct {
	ID        string    `json:"id"`
	TurnID    string    `json:"turn_id"`
	Reason    string    `json:"reason"`
	Channel   string    `json:"channel"`
	Decision  string    `json:"decision"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}
type PromptRecord struct {
	View      PromptView
	RaisedFor string
}

// PromptAPI is the human-facing projection of the existing confirmation service.
// Execution continuations and persistent grants are not exposed here.
type PromptAPI interface {
	Get(string) (PromptRecord, error)
	Resolve(string, bool, string) error
}

func (s *Service) Prompt(ctx context.Context, claims *types.Claims, id string) (PromptView, error) {
	if _, err := caller(ctx, claims); err != nil {
		return PromptView{}, err
	}
	if s.cfg.Prompts == nil {
		return PromptView{}, ErrUnavailable
	}
	rec, err := s.cfg.Prompts.Get(id)
	if err != nil {
		return PromptView{}, err
	}
	if rec.RaisedFor == "" || rec.RaisedFor != claims.UserID {
		return PromptView{}, ErrPromptNotFound
	}
	if rec.View.Decision == "timed_out" || (rec.View.Decision == "pending" && !rec.View.ExpiresAt.IsZero() && time.Now().After(rec.View.ExpiresAt)) {
		return PromptView{}, ErrPromptExpired
	}
	return rec.View, nil
}
func (s *Service) ResolvePrompt(ctx context.Context, claims *types.Claims, id string, approve bool, scope string) (string, string, error) {
	if _, err := s.Prompt(ctx, claims, id); err != nil {
		return "", "", err
	}
	if scope != "session" && scope != "always" {
		scope = "once"
	}
	if err := s.cfg.Prompts.Resolve(id, approve, scope); err != nil {
		return "", "", err
	}
	decision := "denied"
	if approve {
		decision = "approved"
	}
	return decision, scope, nil
}
