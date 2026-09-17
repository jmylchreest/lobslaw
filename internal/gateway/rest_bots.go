package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// BotAPI is what the REST layer needs from the bot registry. Narrow so
// a test can pass a fake without standing up raft.
type BotAPI interface {
	List(ctx context.Context) ([]*lobslawv1.BotRecord, error)
	Get(ctx context.Context, id string) (*lobslawv1.BotRecord, error)
	Put(ctx context.Context, rec *lobslawv1.BotRecord, expectedRevision uint64) (*lobslawv1.BotRecord, error)
	Delete(ctx context.Context, id string) error
}

// InboxAPI is the same for the queues.
type InboxAPI interface {
	List(ctx context.Context, recipient string, f memory.InboxFilter) ([]*lobslawv1.BotInboxItem, error)
	Post(ctx context.Context, item *lobslawv1.BotInboxItem) (*lobslawv1.BotInboxItem, error)
	Get(ctx context.Context, recipient, id string) (*lobslawv1.BotInboxItem, error)
	Cancel(ctx context.Context, recipient, id string) (*lobslawv1.BotInboxItem, error)
	Retry(ctx context.Context, recipient, id string) (*lobslawv1.BotInboxItem, error)
	Recipients(ctx context.Context) ([]string, error)
}

// botJSON is the wire shape.
//
// Hand-written rather than marshalling the proto, for the reason
// planResponseJSON is: a new proto field would otherwise appear in the
// API the moment somebody added it to the record, and the GUI would
// start depending on something nobody decided to publish.
type botJSON struct {
	ID            string   `json:"id"`
	DisplayName   string   `json:"display_name"`
	Description   string   `json:"description"`
	Instructions  string   `json:"instructions"`
	IsCoordinator bool     `json:"is_coordinator"`
	GroupID       string   `json:"group_id"`
	Enabled       bool     `json:"enabled"`
	Tools         []string `json:"tools"`
	MayMessage    []string `json:"may_message"`
	Revision      uint64   `json:"revision"`
	CreatedAt     string   `json:"created_at,omitempty"`
	UpdatedAt     string   `json:"updated_at,omitempty"`
}

type inboxItemJSON struct {
	ID            string `json:"id"`
	Recipient     string `json:"recipient"`
	Sender        string `json:"sender"`
	Kind          string `json:"kind"`
	Subject       string `json:"subject"`
	Body          string `json:"body,omitempty"`
	Priority      int32  `json:"priority"`
	Status        string `json:"status"`
	Result        string `json:"result,omitempty"`
	Error         string `json:"error,omitempty"`
	Attempts      int32  `json:"attempts"`
	CorrelationID string `json:"correlation_id,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	// Who asked for this, as against who put it in the queue.
	RequestedBy string `json:"requested_by,omitempty"`
	// What the turn actually did, as against what its result claims.
	ToolsUsed   []string `json:"tools_used,omitempty"`
	TokensUsed  uint64   `json:"tokens_used,omitempty"`
	CostUSD     float64  `json:"cost_usd,omitempty"`
	CreatedAt   string   `json:"created_at,omitempty"`
	CompletedAt string   `json:"completed_at,omitempty"`
}

func botToJSON(rec *lobslawv1.BotRecord) botJSON {
	out := botJSON{
		ID:            rec.GetId(),
		DisplayName:   rec.GetDisplayName(),
		Description:   rec.GetDescription(),
		Instructions:  rec.GetInstructions(),
		IsCoordinator: rec.GetIsCoordinator(),
		// Resolved, never raw: an empty group_id means the default,
		// and a console that had to know that would get it wrong.
		GroupID:    groupOfBot(rec),
		Enabled:    rec.GetEnabled(),
		Tools:      rec.GetTools(),
		MayMessage: rec.GetMayMessage(),
		Revision:   rec.GetRevision(),
	}
	if ts := rec.GetCreatedAt(); ts != nil {
		out.CreatedAt = ts.AsTime().UTC().Format(rfc3339)
	}
	if ts := rec.GetUpdatedAt(); ts != nil {
		out.UpdatedAt = ts.AsTime().UTC().Format(rfc3339)
	}
	// Non-nil slices so a JSON consumer gets [] rather than null and
	// does not have to special-case "this bot has no edges".
	if out.Tools == nil {
		out.Tools = []string{}
	}
	if out.MayMessage == nil {
		out.MayMessage = []string{}
	}
	return out
}

func inboxToJSON(item *lobslawv1.BotInboxItem, withBody bool) inboxItemJSON {
	out := inboxItemJSON{
		ID:            item.GetId(),
		Recipient:     item.GetRecipient(),
		Sender:        item.GetSender(),
		Kind:          memory.InboxKindName(item.GetKind()),
		Subject:       item.GetSubject(),
		Priority:      item.GetPriority(),
		Status:        memory.InboxStatusName(item.GetStatus()),
		Result:        item.GetResult(),
		Error:         item.GetError(),
		Attempts:      item.GetAttempts(),
		CorrelationID: item.GetCorrelationId(),
		SessionID:     item.GetSessionId(),
		RequestedBy:   item.GetRequestedBy(),
		ToolsUsed:     item.GetToolsUsed(),
		TokensUsed:    item.GetTokensUsed(),
		CostUSD:       item.GetCostUsd(),
	}
	if withBody {
		out.Body = item.GetBody()
	}
	if ts := item.GetCreatedAt(); ts != nil {
		out.CreatedAt = ts.AsTime().UTC().Format(rfc3339)
	}
	if ts := item.GetCompletedAt(); ts != nil {
		out.CompletedAt = ts.AsTime().UTC().Format(rfc3339)
	}
	return out
}

const rfc3339 = "2006-01-02T15:04:05Z07:00"

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
	if _, err := s.authenticate(r); err != nil {
		s.jsonErr(w, http.StatusUnauthorized, err.Error())
		return
	}

	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/bots"), "/")
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
	switch r.Method {
	case http.MethodGet:
		records, err := s.cfg.Bots.List(r.Context())
		if err != nil {
			s.jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := make([]botJSON, 0, len(records))
		for _, rec := range records {
			out = append(out, botToJSON(rec))
		}
		respondJSON(w, http.StatusOK, map[string]any{"bots": out})
	case http.MethodPost:
		var body botJSON
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.jsonErr(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
			return
		}
		rec, err := s.cfg.Bots.Put(r.Context(), &lobslawv1.BotRecord{
			Id:           body.ID,
			DisplayName:  body.DisplayName,
			Description:  body.Description,
			Instructions: body.Instructions,
			Tools:        body.Tools,
			MayMessage:   body.MayMessage,
			Enabled:      true,
			GroupId:      body.GroupID,
			// The real principal, not a placeholder. The session knows
			// who is asking; recording "operator" threw that away at
			// the one point where it is worth keeping.
			CreatedBy: s.principalOf(r),
		}, 0)
		if err != nil {
			s.jsonErr(w, botStatusFor(err), err.Error())
			return
		}
		respondJSON(w, http.StatusCreated, botToJSON(rec))
	default:
		s.jsonErr(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

func (s *Server) handleBotItem(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		rec, err := s.cfg.Bots.Get(r.Context(), id)
		if err != nil {
			s.jsonErr(w, botStatusFor(err), err.Error())
			return
		}
		respondJSON(w, http.StatusOK, botToJSON(rec))
	case http.MethodPatch:
		s.patchBot(w, r, id)
	case http.MethodDelete:
		if !s.mayModifyBot(r, id) {
			s.jsonErr(w, http.StatusForbidden,
				"that bot belongs to somebody else's team")
			return
		}
		if err := s.cfg.Bots.Delete(r.Context(), id); err != nil {
			s.jsonErr(w, botStatusFor(err), err.Error())
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
// mayModifyBot reports whether the caller may change this bot.
//
// Authority comes from the bot's TEAM, not from the bot. Teams gained
// owners in this work and bots did not, which left the team check
// bypassable one route over: a bot you could not move between teams
// you could still re-brief, re-tool, or delete — including the
// coordinator, which decides who answers on Telegram for that team.
//
// A node with no group registry falls back to "anyone signed in",
// which is the behaviour before teams existed.
func (s *Server) mayModifyBot(r *http.Request, botID string) bool {
	if s.cfg.Groups == nil || s.cfg.Bots == nil {
		return true
	}
	rec, err := s.cfg.Bots.Get(r.Context(), botID)
	if err != nil {
		// Let the caller's own lookup produce the proper 404 rather
		// than reporting a missing bot as a permissions problem.
		return true
	}
	group, err := s.cfg.Groups.Get(r.Context(), groupOfBot(rec))
	if err != nil {
		// A bot pointing at a team that is not there is not somebody
		// else's team.
		return true
	}
	return groupMayModify(group, s.principalOf(r))
}

func (s *Server) patchBot(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		DisplayName  *string   `json:"display_name"`
		Description  *string   `json:"description"`
		Instructions *string   `json:"instructions"`
		Tools        *[]string `json:"tools"`
		MayMessage   *[]string `json:"may_message"`
		Enabled      *bool     `json:"enabled"`
		GroupID      *string   `json:"group_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.jsonErr(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
		return
	}
	current, err := s.cfg.Bots.Get(r.Context(), id)
	if err != nil {
		s.jsonErr(w, botStatusFor(err), err.Error())
		return
	}
	if !s.mayModifyBot(r, id) {
		s.jsonErr(w, http.StatusForbidden, "that bot belongs to somebody else's team")
		return
	}
	if body.DisplayName != nil {
		current.DisplayName = *body.DisplayName
	}
	if body.Description != nil {
		current.Description = *body.Description
	}
	if body.Instructions != nil {
		current.Instructions = *body.Instructions
	}
	if body.Tools != nil {
		current.Tools = *body.Tools
	}
	if body.MayMessage != nil {
		current.MayMessage = *body.MayMessage
	}
	if body.Enabled != nil {
		current.Enabled = *body.Enabled
	}
	if body.GroupID != nil {
		// Moving a bot between teams is an ordinary edit. The
		// coordinator is the exception: it is what a channel reaches
		// for its group, so moving it would leave that team
		// unreachable and the next inbound message unanswered.
		if current.GetIsCoordinator() {
			s.jsonErr(w, http.StatusBadRequest,
				"the coordinator belongs to its own team and cannot be moved; "+
					"make another bot the coordinator there first")
			return
		}
		current.GroupId = *body.GroupID
	}
	updated, err := s.cfg.Bots.Put(r.Context(), current, current.GetRevision())
	if err != nil {
		s.jsonErr(w, botStatusFor(err), err.Error())
		return
	}
	s.auditRegistry(r, "bot:update", id, updated.GetDisplayName())
	respondJSON(w, http.StatusOK, botToJSON(updated))
}

func (s *Server) handleBotInbox(w http.ResponseWriter, r *http.Request, botID string) {
	if s.cfg.Inbox == nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "this node does not host the bot inbox")
		return
	}
	switch r.Method {
	case http.MethodGet:
		filter := memory.InboxFilter{Limit: 100}
		if raw := r.URL.Query().Get("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				filter.Limit = n
			}
		}
		if raw := r.URL.Query().Get("status"); raw != "" && raw != "all" {
			status, ok := parseRESTInboxStatus(raw)
			if !ok {
				s.jsonErr(w, http.StatusBadRequest, "unknown status "+strconv.Quote(raw))
				return
			}
			filter.Statuses = []lobslawv1.InboxStatus{status}
		}
		items, err := s.cfg.Inbox.List(r.Context(), botID, filter)
		if err != nil {
			s.jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := make([]inboxItemJSON, 0, len(items))
		for _, item := range items {
			out = append(out, inboxToJSON(item, false))
		}
		respondJSON(w, http.StatusOK, map[string]any{"bot": botID, "items": out})
	case http.MethodPost:
		var body struct {
			Subject  string `json:"subject"`
			Body     string `json:"body"`
			Kind     string `json:"kind"`
			Priority int32  `json:"priority"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.jsonErr(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
			return
		}
		kind, ok := parseRESTInboxKind(body.Kind)
		if !ok {
			s.jsonErr(w, http.StatusBadRequest, "unknown kind "+strconv.Quote(body.Kind))
			return
		}
		// Who assigned this, from the session rather than the body:
		// the browser is not the authority on who is asking. Now that
		// the console can tell people apart, an operator has a name,
		// and it follows the work — so a report this produces can say
		// who asked for it even after it has changed hands.
		requester := "operator"
		if claims, aerr := s.authenticate(r); aerr == nil && claims != nil && claims.UserID != "" {
			requester = claims.UserID
		}
		item, err := s.cfg.Inbox.Post(r.Context(), &lobslawv1.BotInboxItem{
			Recipient: botID,
			// An operator assigning work from the GUI is the sender.
			Sender:      "operator",
			RequestedBy: requester,
			Kind:        kind,
			Subject:     body.Subject,
			Body:        body.Body,
			Priority:    body.Priority,
		})
		if err != nil {
			s.jsonErr(w, inboxStatusFor(err), err.Error())
			return
		}
		respondJSON(w, http.StatusCreated, inboxToJSON(item, true))
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
	if _, err := s.authenticate(r); err != nil {
		s.jsonErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/inbox"), "/")
	botID, itemID, ok := strings.Cut(rest, "/")
	if !ok || botID == "" || itemID == "" {
		s.jsonErr(w, http.StatusBadRequest, "want /v1/inbox/{bot}/{item}")
		return
	}

	switch r.Method {
	case http.MethodGet:
		item, err := s.cfg.Inbox.Get(r.Context(), botID, itemID)
		if err != nil {
			s.jsonErr(w, inboxStatusFor(err), err.Error())
			return
		}
		respondJSON(w, http.StatusOK, inboxToJSON(item, true))
	case http.MethodPatch:
		var body struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.jsonErr(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
			return
		}
		var (
			item *lobslawv1.BotInboxItem
			err  error
		)
		switch strings.ToLower(strings.TrimSpace(body.Action)) {
		case "retry":
			item, err = s.cfg.Inbox.Retry(r.Context(), botID, itemID)
		case "cancel":
			item, err = s.cfg.Inbox.Cancel(r.Context(), botID, itemID)
		default:
			s.jsonErr(w, http.StatusBadRequest, `action must be "retry" or "cancel"`)
			return
		}
		if err != nil {
			s.jsonErr(w, inboxStatusFor(err), err.Error())
			return
		}
		respondJSON(w, http.StatusOK, inboxToJSON(item, true))
	default:
		s.jsonErr(w, http.StatusMethodNotAllowed, "GET or PATCH")
	}
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
	if _, err := s.authenticate(r); err != nil {
		s.jsonErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if r.Method != http.MethodGet {
		s.jsonErr(w, http.StatusMethodNotAllowed, "GET")
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}

	recipients, err := s.cfg.Inbox.Recipients(r.Context())
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]inboxItemJSON, 0, limit)
	for _, recipient := range recipients {
		items, err := s.cfg.Inbox.List(r.Context(), recipient, memory.InboxFilter{Limit: limit})
		if err != nil {
			s.jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, item := range items {
			out = append(out, inboxToJSON(item, false))
		}
	}
	// Newest first across every queue. Ids are ULIDs, so descending id
	// is descending time without reading a clock or a timestamp that
	// might be absent.
	sortInboxDescending(out)
	if len(out) > limit {
		out = out[:limit]
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": out})
}

func sortInboxDescending(items []inboxItemJSON) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].ID > items[j-1].ID; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
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
	case errors.Is(err, memory.ErrBotNotFound):
		return http.StatusNotFound
	case errors.Is(err, memory.ErrClaimConflict):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

func inboxStatusFor(err error) int {
	switch {
	case errors.Is(err, memory.ErrInboxNotFound):
		return http.StatusNotFound
	case errors.Is(err, memory.ErrInboxFull):
		// 429, not 400: the request was well-formed and the answer is
		// "come back when this bot has caught up".
		return http.StatusTooManyRequests
	case errors.Is(err, memory.ErrClaimConflict):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

func parseRESTInboxStatus(s string) (lobslawv1.InboxStatus, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "pending":
		return lobslawv1.InboxStatus_INBOX_STATUS_PENDING, true
	case "claimed":
		return lobslawv1.InboxStatus_INBOX_STATUS_CLAIMED, true
	case "done":
		return lobslawv1.InboxStatus_INBOX_STATUS_DONE, true
	case "failed":
		return lobslawv1.InboxStatus_INBOX_STATUS_FAILED, true
	case "cancelled", "canceled":
		return lobslawv1.InboxStatus_INBOX_STATUS_CANCELLED, true
	default:
		return lobslawv1.InboxStatus_INBOX_STATUS_UNSPECIFIED, false
	}
}

func parseRESTInboxKind(s string) (lobslawv1.InboxKind, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "task":
		return lobslawv1.InboxKind_INBOX_KIND_TASK, true
	case "question":
		return lobslawv1.InboxKind_INBOX_KIND_QUESTION, true
	case "fyi":
		return lobslawv1.InboxKind_INBOX_KIND_FYI, true
	default:
		return lobslawv1.InboxKind_INBOX_KIND_UNSPECIFIED, false
	}
}
