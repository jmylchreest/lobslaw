package gateway

import (
	"context"
	"net/http"
	"strings"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// SessionBrowser is the read-only slice of the session service the
// console needs. Read-only deliberately: the console shows what a bot
// did, and a transcript that an operator can edit is not a record of
// anything.
type SessionBrowser interface {
	ListFiltered(ctx context.Context, channel, userID string) ([]*lobslawv1.SessionRecord, error)
	LoadMessages(ctx context.Context, id string) ([]*lobslawv1.SessionMessage, error)
}

type sessionJSON struct {
	ID        string `json:"id"`
	Channel   string `json:"channel"`
	ChannelID string `json:"channel_id"`
	Title     string `json:"title,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	Messages  uint64 `json:"messages"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type messageJSON struct {
	Seq       uint64 `json:"seq"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	ToolCalls int    `json:"tool_calls,omitempty"`
	TurnID    string `json:"turn_id,omitempty"`
}

func sessionToJSON(rec *lobslawv1.SessionRecord) sessionJSON {
	out := sessionJSON{
		ID:        rec.GetId(),
		Channel:   rec.GetChannel(),
		ChannelID: rec.GetChannelId(),
		Title:     rec.GetTitle(),
		UserID:    rec.GetUserId(),
		Messages:  rec.GetNextSeq(),
	}
	if ts := rec.GetUpdatedAt(); ts != nil {
		out.UpdatedAt = ts.AsTime().UTC().Format(rfc3339)
	}
	return out
}

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
	records, err := s.cfg.Transcripts.ListFiltered(r.Context(), botChannel, "")
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]sessionJSON, 0, len(records))
	for _, rec := range records {
		if !belongsToBot(rec.GetChannelId(), botID) {
			continue
		}
		out = append(out, sessionToJSON(rec))
	}
	respondJSON(w, http.StatusOK, map[string]any{"bot": botID, "sessions": out})
}

// belongsToBot reports whether a channel id is one of this bot's.
//
// Prefix match on a separator rather than HasPrefix(id, bot), because
// "eng" would otherwise claim "engineering"'s conversations — the kind
// of leak that only shows up once somebody names two bots similarly.
func belongsToBot(channelID, botID string) bool {
	return channelID == botID || strings.HasPrefix(channelID, botID+".")
}

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

	messages, err := s.cfg.Transcripts.LoadMessages(r.Context(), id)
	if err != nil {
		s.jsonErr(w, http.StatusNotFound, err.Error())
		return
	}
	out := make([]messageJSON, 0, len(messages))
	for _, m := range messages {
		out = append(out, messageJSON{
			Seq:       m.GetSeq(),
			Role:      m.GetRole(),
			Content:   m.GetContent(),
			ToolCalls: len(m.GetToolCalls()),
			TurnID:    m.GetTurnId(),
		})
	}
	respondJSON(w, http.StatusOK, map[string]any{"id": id, "messages": out})
}
