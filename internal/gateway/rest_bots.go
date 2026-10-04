package gateway

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/bots"
	"github.com/jmylchreest/lobslaw/internal/console"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// BotAPI is what the REST layer needs from the bot registry. Narrow so
// a test can pass a fake without standing up raft.
type BotAPI = console.BotAPI
type InboxAPI = console.InboxAPI

// botJSON is the wire shape.
//
// Hand-written rather than marshalling the proto, for the reason
// planResponseJSON is: a new proto field would otherwise appear in the
// API the moment somebody added it to the record, and the GUI would
// start depending on something nobody decided to publish.
type botJSON = console.BotView
type inboxItemJSON = console.InboxItemView

// handleBots serves /v1/bots and /v1/bots/{id}[/inbox].
//
// One handler over a path prefix rather than a router: the gateway
// mux is net/http's, the shapes are few, and adding a routing
// dependency for six paths would be a bigger decision than the six
// paths are.
func (s *Server) handleBots(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Bots == nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "this node does not host the bot registry")
		return
	}
	authn, err := s.authenticateRequest(r)
	if err != nil {
		s.jsonErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if err := s.checkCookieCSRF(r, authn); err != nil {
		s.jsonErr(w, http.StatusForbidden, err.Error())
		return
	}

	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/bots"), "/")
	if rest != "" {
		botID, _, _ := strings.Cut(rest, "/")
		if !s.mayModifyBot(r, botID) {
			s.jsonErr(w, http.StatusForbidden, "that bot is not owned by this account")
			return
		}
	}
	switch {
	case rest == "":
		s.handleBotCollection(w, r)
	case strings.HasSuffix(rest, "/inbox"):
		s.handleBotInbox(w, r, strings.TrimSuffix(rest, "/inbox"))
	case strings.HasSuffix(rest, "/messages"):
		s.handleBotChat(w, r, strings.TrimSuffix(rest, "/messages"))
	case strings.HasSuffix(rest, "/sessions"):
		s.handleBotSessions(w, r, strings.TrimSuffix(rest, "/sessions"))
	case strings.HasSuffix(rest, "/routines"):
		s.handleBotRoutines(w, r, strings.TrimSuffix(rest, "/routines"))
	case strings.HasSuffix(rest, "/memory"):
		s.handleBotMemory(w, r, strings.TrimSuffix(rest, "/memory"))
	default:
		s.handleBotItem(w, r, rest)
	}
}

func (s *Server) handleBotCollection(w http.ResponseWriter, r *http.Request) {
	claims := s.consoleClaims(r)
	switch r.Method {
	case http.MethodGet:
		rows, err := s.consoleOperations().Bots(r.Context(), claims)
		if err != nil {
			s.consoleOperationError(w, err, http.StatusInternalServerError)
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"bots": rows})
	case http.MethodPost:
		var body botJSON
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.jsonErr(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
			return
		}
		row, err := s.consoleOperations().CreateBot(r.Context(), claims, body)
		if err != nil {
			s.consoleOperationError(w, err, botStatusFor(err))
			return
		}
		respondJSON(w, http.StatusCreated, row)
	default:
		s.jsonErr(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

func (s *Server) handleBotItem(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		row, err := s.consoleOperations().Bot(r.Context(), s.consoleClaims(r), id)
		if err != nil {
			s.consoleOperationError(w, err, botStatusFor(err))
			return
		}
		respondJSON(w, http.StatusOK, row)
	case http.MethodPatch:
		s.patchBot(w, r, id)
	case http.MethodDelete:
		if err := s.consoleOperations().DeleteBot(r.Context(), s.consoleClaims(r), id); err != nil {
			s.consoleOperationError(w, err, botStatusFor(err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		s.jsonErr(w, http.StatusMethodNotAllowed, "GET, PATCH or DELETE")
	}
}

// patchBot applies a partial update.
//
// Pointers so an omitted field is left alone rather than cleared —
// PATCH with a plain struct would make "did not mention instructions"
// indistinguishable from "set instructions to empty", and clearing a
// bot's brief by editing its display name is the kind of surprise a
// GUI should be incapable of.
// mayUseGroup reports whether the caller may put a bot into a team.
//
// The destination half of the question. mayModifyBot answers "may you
// touch this bot", which is about where it IS — and on a move the
// interesting team is the one it is going TO. Without this, creating
// a bot inside somebody else's team, or moving one into it, both
// walked straight past the guard that exists to stop exactly that.
func (s *Server) mayUseGroup(r *http.Request, groupID string) bool {
	if s.cfg.Groups == nil {
		return false
	}
	if strings.TrimSpace(groupID) == "" {
		return false
	}
	group, err := s.cfg.Groups.Get(r.Context(), groupID)
	if err != nil {
		return false
	}
	return groupMayModify(group, s.principalOf(r))
}

// Bot ownership is authoritative even when the group record is unavailable.
// Group membership must never make an unowned bot public.
func (s *Server) mayModifyBot(r *http.Request, botID string) bool {
	if s.cfg.Bots == nil {
		return false
	}
	rec, err := s.cfg.Bots.Get(r.Context(), botID)
	if err != nil {
		return false
	}
	return bots.MayModify(rec, s.principalOf(r))
}

func (s *Server) patchBot(w http.ResponseWriter, r *http.Request, id string) {
	var body console.BotPatch
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.jsonErr(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
		return
	}
	row, err := s.consoleOperations().UpdateBot(r.Context(), s.consoleClaims(r), id, body)
	if err != nil {
		s.consoleOperationError(w, err, botStatusFor(err))
		return
	}
	respondJSON(w, http.StatusOK, row)
}

func (s *Server) handleBotInbox(w http.ResponseWriter, r *http.Request, botID string) {
	if s.cfg.Inbox == nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "this node does not host the bot inbox")
		return
	}
	switch r.Method {
	case http.MethodGet:
		limit := 100
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				s.jsonErr(w, http.StatusBadRequest, "invalid limit")
				return
			}
			limit = n
		}
		rows, err := s.consoleOperations().Inbox(r.Context(), s.consoleClaims(r), botID, r.URL.Query().Get("status"), limit)
		if err != nil {
			s.consoleOperationError(w, err, http.StatusInternalServerError)
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"bot": botID, "items": rows})
	case http.MethodPost:
		var body console.InboxInput
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.jsonErr(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
			return
		}
		row, err := s.consoleOperations().PostInbox(r.Context(), s.consoleClaims(r), botID, body)
		if err != nil {
			s.consoleOperationError(w, err, inboxStatusFor(err))
			return
		}
		respondJSON(w, http.StatusCreated, row)
	default:
		s.jsonErr(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

// handleInboxItem serves PATCH /v1/inbox/{bot}/{id} — retry, cancel,
// or read one item in full.
func (s *Server) handleInboxItem(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Inbox == nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "this node does not host the bot inbox")
		return
	}
	if _, err := s.authenticateRequest(r); err != nil {
		s.jsonErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/inbox"), "/")
	botID, itemID, ok := strings.Cut(rest, "/")
	if !ok || botID == "" || itemID == "" {
		s.jsonErr(w, http.StatusBadRequest, "want /v1/inbox/{bot}/{item}")
		return
	}
	if !s.mayModifyBot(r, botID) {
		s.jsonErr(w, http.StatusForbidden, "that bot is not owned by this account")
		return
	}
	authn, _ := s.authenticateRequest(r)
	if err := s.checkCookieCSRF(r, authn); err != nil {
		s.jsonErr(w, http.StatusForbidden, err.Error())
		return
	}

	switch r.Method {
	case http.MethodGet:
		row, err := s.consoleOperations().InboxItem(r.Context(), authn.Claims, botID, itemID)
		if err != nil {
			s.consoleOperationError(w, err, inboxStatusFor(err))
			return
		}
		respondJSON(w, http.StatusOK, row)
	case http.MethodPatch:
		var body struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.jsonErr(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
			return
		}
		row, err := s.consoleOperations().ChangeInbox(r.Context(), authn.Claims, botID, itemID, body.Action)
		if err != nil {
			s.consoleOperationError(w, err, inboxStatusFor(err))
			return
		}
		respondJSON(w, http.StatusOK, row)
	default:
		s.jsonErr(w, http.StatusMethodNotAllowed, "GET or PATCH")
	}
}

const (
	defaultActivityLimit int = 100
	maxActivityLimit     int = bots.MaxInboxRecentItems
)

func activityLimit(raw string) (int, error) {
	if raw == "" {
		return defaultActivityLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > maxActivityLimit {
		return 0, fmt.Errorf("activity limit must be between 0 and %d", maxActivityLimit)
	}
	if n == 0 {
		return defaultActivityLimit, nil
	}
	return n, nil
}

// handleActivity is the cross-bot timeline: every queue, newest first.
//
// The GUI's front page. Built from the inbox rather than from sessions
// because the question it answers is "what is the team doing", and a
// session index answers "what conversations exist" — which is not the
// same thing once bots start working items nobody chatted about.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Inbox == nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "this node does not host the bot inbox")
		return
	}
	if _, err := s.authenticateRequest(r); err != nil {
		s.jsonErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if r.Method != http.MethodGet {
		s.jsonErr(w, http.StatusMethodNotAllowed, "GET")
		return
	}
	limit, err := activityLimit(r.URL.Query().Get("limit"))
	if err != nil {
		s.jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}

	rows, err := s.consoleOperations().Activity(r.Context(), s.consoleClaims(r), limit)
	if err != nil {
		s.consoleOperationError(w, err, http.StatusInternalServerError)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": rows})
}

func sortInboxDescending(items []inboxItemJSON) {
	slices.SortFunc(items, func(a, b inboxItemJSON) int { return cmp.Compare(b.ID, a.ID) })
}

// Keep only the newest candidates between bounded batches, including when an
// InboxAPI implementation returns more than the requested per-bot limit.
func collectActivity(out []inboxItemJSON, items []*lobslawv1.ConsoleInboxItem, limit int) []inboxItemJSON {
	for len(items) > 0 {
		n := min(limit, len(items))
		for _, item := range items[:n] {
			out = append(out, inboxSummaryToJSON(item))
		}
		sortInboxDescending(out)
		if len(out) > limit {
			clear(out[limit:])
			out = out[:limit]
		}
		items = items[n:]
	}
	return out
}

func inboxSummaryToJSON(item *lobslawv1.ConsoleInboxItem) inboxItemJSON {
	return inboxItemJSON{
		ID: item.GetId(), Recipient: item.GetRecipient(), Sender: item.GetSender(),
		Kind: item.GetKind(), Subject: item.GetSubject(), Priority: item.GetPriority(),
		Status: item.GetStatus(), Result: item.GetResult(), Error: item.GetError(),
		Attempts: item.GetAttempts(), CorrelationID: item.GetCorrelationId(),
		SessionID: item.GetSessionId(), TaskID: item.GetTaskId(), RequestedBy: item.GetRequestedBy(),
		ToolsUsed: item.GetToolsUsed(), TokensUsed: item.GetTokensUsed(), CostUSD: item.GetCostUsd(),
		CreatedAt: item.GetCreatedAt(), CompletedAt: item.GetCompletedAt(),
		Revision: item.GetRevision(), TruncatedFields: item.GetTruncatedFields(), DetailPath: item.GetDetailPath(),
	}
}

func respondJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// botStatusFor maps a registry error to an HTTP status so a GUI can
// tell "you typed a name that does not exist" from "somebody else
// edited this while you had the form open".
func botStatusFor(err error) int {
	switch {
	case errors.Is(err, bots.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, types.ErrConflict):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

func inboxStatusFor(err error) int {
	switch {
	case errors.Is(err, bots.ErrInboxNotFound):
		return http.StatusNotFound
	case errors.Is(err, bots.ErrInboxFull):
		// 429, not 400: the request was well-formed and the answer is
		// "come back when this bot has caught up".
		return http.StatusTooManyRequests
	case errors.Is(err, types.ErrConflict):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

func (s *Server) registerTeamRoutes(mux *http.ServeMux) {
	if s.cfg.Bots != nil || s.cfg.RemoteConsole != nil {
		mux.HandleFunc("/v1/bots", s.consoleRoute(s.handleBots))
		mux.HandleFunc("/v1/bots/", s.consoleRoute(s.handleBots))
		mux.HandleFunc("/v1/inbox/", s.consoleRoute(s.handleInboxItem))
		mux.HandleFunc("/v1/activity", s.consoleRoute(s.handleActivity))
	}
	if s.cfg.Groups != nil || s.cfg.RemoteConsole != nil {
		mux.HandleFunc("/v1/groups", s.consoleRoute(s.handleGroups))
		mux.HandleFunc("/v1/groups/", s.consoleRoute(s.handleGroups))
	}
}

func (s *Server) resolveTeamBot(ctx context.Context, channel, channelID, userID string) string {
	if s.cfg.TeamRouter == nil {
		return ""
	}
	return s.cfg.TeamRouter.BotForChannel(ctx, channel, channelID, userID)
}
