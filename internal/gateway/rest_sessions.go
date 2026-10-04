package gateway

import (
	"net/http"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/console"
)

// SessionBrowser is the read-only slice of the session service the
// console needs. Read-only deliberately: the console shows what a bot
// did, and a transcript that an operator can edit is not a record of
// anything.
type SessionBrowser = console.SessionBrowser

// botChannel is the synthetic channel name a bot's own conversations
// are filed under, distinct from telegram / slack / rest so a bot's
// working transcripts never appear in somebody's chat history.
const botChannel = "bot"

// handleBotSessions serves GET /v1/bots/{id}/sessions.
//
// A bot's conversations are the ones on the synthetic "bot" channel
// whose channel id it prefixes. Filtering here rather than adding a
// bot field to SessionRecord: the transcript store's key already
// encodes which conversation a turn belongs to, and a second field
// saying the same thing is a second field that can disagree.
func (s *Server) handleBotSessions(w http.ResponseWriter, r *http.Request, botID string) {
	if s.cfg.Transcripts == nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "this node does not host the transcript store")
		return
	}
	if r.Method != http.MethodGet {
		s.jsonErr(w, http.StatusMethodNotAllowed, "GET")
		return
	}
	rows, err := s.consoleOperations().Sessions(r.Context(), s.consoleClaims(r), botID)
	if err != nil {
		s.consoleOperationError(w, err, http.StatusInternalServerError)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"bot": botID, "sessions": rows})
}

// belongsToBot reports whether a channel id is one of this bot's.
//
// Prefix match on a separator rather than HasPrefix(id, bot), because
// "eng" would otherwise claim "engineering"'s conversations — the kind
// of leak that only shows up once somebody names two bots similarly.

// handleSessionTranscript serves GET /v1/sessions/{id} — one
// transcript. Named apart from the /v1/session login handler, which is
// a different thing entirely.
//
// This is what an inbox item's session_id points at. Without it the
// console's answer to "what did the bot actually do" stops at the
// result string, and the turn that produced it is unreachable.
func (s *Server) handleSessionTranscript(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticateRequest(r); err != nil {
		s.jsonErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if s.cfg.Transcripts == nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "this node does not host the transcript store")
		return
	}
	if r.Method != http.MethodGet {
		s.jsonErr(w, http.StatusMethodNotAllowed, "GET")
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/sessions"), "/")
	if id == "" {
		s.jsonErr(w, http.StatusBadRequest, "want /v1/sessions/{id}")
		return
	}

	rows, err := s.consoleOperations().Transcript(r.Context(), s.consoleClaims(r), id)
	if err != nil {
		s.consoleOperationError(w, err, http.StatusNotFound)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"id": id, "messages": rows})
}
