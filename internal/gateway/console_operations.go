package gateway

import (
	"errors"
	"net/http"

	"github.com/jmylchreest/lobslaw/internal/console"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Transport authentication supplies claims; operations repeat resource authorization.
func (s *Server) consoleOperations() *console.Service {
	return console.New(console.Config{Bots: s.cfg.Bots, Groups: s.cfg.Groups, Inbox: s.cfg.Inbox, Transcripts: s.cfg.Transcripts, Routines: s.cfg.Routines, Memory: s.cfg.Memory, Tools: s.cfg.Tools, Plan: s.cfg.Plan, Prompts: consolePromptAdapter{s.cfg.Prompts}, Logger: s.log})
}
func (s *Server) consoleClaims(r *http.Request) *types.Claims {
	authn, err := s.authenticateRequest(r)
	if err != nil {
		return nil
	}
	return authn.Claims
}
func (s *Server) consoleOperationError(w http.ResponseWriter, err error, fallback int) {
	switch {
	case errors.Is(err, console.ErrForbidden):
		fallback = http.StatusForbidden
	case errors.Is(err, console.ErrUnauthenticated):
		fallback = http.StatusUnauthorized
	case errors.Is(err, console.ErrUnavailable):
		fallback = http.StatusServiceUnavailable
	case errors.Is(err, console.ErrConflict):
		fallback = http.StatusConflict
	case errors.Is(err, console.ErrInvalid):
		fallback = http.StatusBadRequest
	}
	s.jsonErr(w, fallback, err.Error())
}

type consolePromptAdapter struct{ inner Prompts }

func (a consolePromptAdapter) Get(id string) (console.PromptRecord, error) {
	if a.inner == nil {
		return console.PromptRecord{}, console.ErrUnavailable
	}
	p, err := a.inner.Get(id)
	if err != nil {
		return console.PromptRecord{}, err
	}
	return console.PromptRecord{RaisedFor: p.RaisedFor, View: console.PromptView{ID: p.ID, TurnID: p.TurnID, Reason: p.Reason, Channel: p.Channel, Decision: p.Decision.String(), CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt}}, nil
}
func (a consolePromptAdapter) Resolve(id string, approve bool, scope string) error {
	if a.inner == nil {
		return console.ErrUnavailable
	}
	decision := PromptDenied
	if approve {
		decision = PromptApproved
	}
	return a.inner.Resolve(id, decision, ParsePromptScope(scope))
}

func (s *Server) consolePromptError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, console.ErrPromptNotFound):
		code = http.StatusNotFound
	case errors.Is(err, console.ErrPromptExpired), errors.Is(err, console.ErrPromptResolved):
		code = http.StatusConflict
	}
	s.consoleOperationError(w, err, code)
}
