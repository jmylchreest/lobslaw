package gateway

import (
	"net/http"

	"github.com/jmylchreest/lobslaw/internal/console"
)

// RoutineAPI lists the scheduled work a principal owns. Implemented by
// the node over the replicated scheduler store.
//
// A gateway-local interface for the same reason as BotAPI: the gateway
// should not import the scheduler to render a list.
type RoutineAPI = console.RoutineAPI
type MemoryAPI = console.MemoryAPI
type MemoryRecordView = console.MemoryRecordView

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
	rows, err := s.consoleOperations().Routines(r.Context(), s.consoleClaims(r), botID)
	if err != nil {
		s.consoleOperationError(w, err, http.StatusInternalServerError)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"routines": rows})
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
	rows, total, err := s.consoleOperations().Memory(r.Context(), s.consoleClaims(r), botID)
	if err != nil {
		s.consoleOperationError(w, err, http.StatusInternalServerError)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"records": rows, "total": total})
}
