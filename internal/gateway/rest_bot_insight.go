package gateway

import (
	"context"
	"net/http"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// RoutineAPI lists the scheduled work a principal owns. Implemented by
// the node over the replicated scheduler store.
//
// A gateway-local interface for the same reason as BotAPI: the gateway
// should not import the scheduler to render a list.
type RoutineAPI interface {
	TasksForOwner(owner string) ([]*lobslawv1.ScheduledTaskRecord, error)
}

// MemoryAPI is a bot's own memory, read-only.
//
// Read-only deliberately. Editing what a bot remembers from a console
// is a different feature with a different risk profile, and shipping
// the viewer first answers the question people actually have — "why
// did it say that" — without offering a way to rewrite the evidence.
type MemoryAPI interface {
	RecordsForOwner(ctx context.Context, owner string, limit int) ([]MemoryRecordView, int, error)
}

// MemoryRecordView is one remembered thing, flattened.
type MemoryRecordView struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Text      string   `json:"text"`
	Tags      []string `json:"tags,omitempty"`
	Scope     string   `json:"scope,omitempty"`
	CreatedAt string   `json:"created_at,omitempty"`
}

type routineJSON struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Handler  string `json:"handler_ref"`
	Enabled  bool   `json:"enabled"`
	LastRun  string `json:"last_run,omitempty"`
	NextRun  string `json:"next_run,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
}

// handleBotRoutines serves GET /v1/bots/{id}/routines.
//
// Scoped by the bot's principal rather than by a query parameter, so
// one bot's routines cannot be listed by asking for another's.
func (s *Server) handleBotRoutines(w http.ResponseWriter, r *http.Request, botID string) {
	if r.Method != http.MethodGet {
		s.jsonErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if s.cfg.Routines == nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "this node does not run the scheduler")
		return
	}
	tasks, err := s.cfg.Routines.TasksForOwner("bot:" + botID)
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]routineJSON, 0, len(tasks))
	for _, t := range tasks {
		row := routineJSON{
			ID:       t.GetId(),
			Name:     t.GetName(),
			Schedule: t.GetSchedule(),
			Handler:  t.GetHandlerRef(),
			Enabled:  t.GetEnabled(),
			// The instruction the routine will run, so "check the
			// cluster daily" is legible without opening the record.
			Prompt: t.GetParams()["prompt"],
		}
		if ts := t.GetLastRun(); ts != nil {
			row.LastRun = ts.AsTime().UTC().Format(rfc3339)
		}
		if ts := t.GetNextRun(); ts != nil {
			row.NextRun = ts.AsTime().UTC().Format(rfc3339)
		}
		out = append(out, row)
	}
	respondJSON(w, http.StatusOK, map[string]any{"routines": out})
}

// handleBotMemory serves GET /v1/bots/{id}/memory.
func (s *Server) handleBotMemory(w http.ResponseWriter, r *http.Request, botID string) {
	if r.Method != http.MethodGet {
		s.jsonErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if s.cfg.Memory == nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "this node hosts no memory state")
		return
	}
	records, total, err := s.cfg.Memory.RecordsForOwner(r.Context(), "bot:"+botID, 100)
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if records == nil {
		records = []MemoryRecordView{}
	}
	// total is the pre-limit count so the console can say "showing 100
	// of 400" rather than implying 100 is everything.
	respondJSON(w, http.StatusOK, map[string]any{"records": records, "total": total})
}
