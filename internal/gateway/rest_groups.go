package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/bots"
	"github.com/jmylchreest/lobslaw/internal/console"
	"github.com/jmylchreest/lobslaw/internal/identity"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// GroupAPI is the teams registry as the gateway needs it.
type GroupAPI = console.GroupAPI
type groupJSON = console.GroupView

// handleGroups serves /v1/groups and /v1/groups/{id}.
func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Groups == nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "this node does not host the group registry")
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
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/v1/groups"), "/")

	switch {
	case rest == "" && r.Method == http.MethodGet:
		s.listGroups(w, r)
	case rest == "" && r.Method == http.MethodPost:
		s.writeGroup(w, r, "", 0)
	case rest != "" && r.Method == http.MethodGet:
		s.getGroup(w, r, rest)
	case rest != "" && r.Method == http.MethodPatch:
		s.patchGroup(w, r, rest)
	case rest != "" && r.Method == http.MethodDelete:
		if err := s.consoleOperations().DeleteGroup(r.Context(), s.consoleClaims(r), rest); err != nil {
			s.consoleOperationError(w, err, groupStatusFor(err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		s.jsonErr(w, http.StatusMethodNotAllowed, "unsupported method for this path")
	}
}

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request) {
	rows, err := s.consoleOperations().Groups(r.Context(), s.consoleClaims(r))
	if err != nil {
		s.consoleOperationError(w, err, groupStatusFor(err))
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"groups": rows})
}

func (s *Server) getGroup(w http.ResponseWriter, r *http.Request, id string) {
	row, err := s.consoleOperations().Group(r.Context(), s.consoleClaims(r), id)
	if err != nil {
		s.consoleOperationError(w, err, groupStatusFor(err))
		return
	}
	respondJSON(w, http.StatusOK, row)
}

type groupBody = console.GroupInput

func (s *Server) writeGroup(w http.ResponseWriter, r *http.Request, id string, _ uint64) {
	var body groupBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.jsonErr(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
		return
	}
	if id != "" {
		body.ID = id
	}
	row, err := s.consoleOperations().CreateGroup(r.Context(), s.consoleClaims(r), body)
	if err != nil {
		s.consoleOperationError(w, err, groupStatusFor(err))
		return
	}
	respondJSON(w, http.StatusCreated, row)
}

func (s *Server) patchGroup(w http.ResponseWriter, r *http.Request, id string) {
	var body groupBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.jsonErr(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
		return
	}
	row, err := s.consoleOperations().UpdateGroup(r.Context(), s.consoleClaims(r), id, body)
	if err != nil {
		s.consoleOperationError(w, err, groupStatusFor(err))
		return
	}
	respondJSON(w, http.StatusOK, row)
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// groupOfBot mirrors bots.GroupOf: an empty group_id means the
// default, so bots written before groups existed still appear in a
// team rather than in none.

func groupStatusFor(err error) int {
	switch {
	case strings.Contains(err.Error(), "groups: not found"):
		return http.StatusNotFound
	case strings.Contains(err.Error(), "changed; read it again"):
		return http.StatusConflict
	case strings.Contains(err.Error(), "cannot be deleted"),
		strings.Contains(err.Error(), "is required"),
		strings.Contains(err.Error(), "must be lowercase"):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// groupMayModify mirrors bots.MayModifyGroup, which fails closed: an
// empty principal is nobody and an empty owner is nobody, so an
// unowned team is inaccessible rather than public. Ownership must have
// been established explicitly (see ensureOwnersTeam).
func groupMayModify(rec *lobslawv1.GroupRecord, principal string) bool {
	return bots.MayModifyGroup(rec, principal)
}

// principalOf is who is making this request, or empty for an
// unauthenticated one.
func (s *Server) principalOf(r *http.Request) string {
	authn, err := s.authenticateRequest(r)
	if err != nil || authn.Claims == nil {
		return ""
	}
	return canonicalUserPrincipal(authn.Claims.UserID)
}

// canonicalUserPrincipal spells a channel's user id as the ownership
// principal "user:<id>". REST sessions carry the bare id while bot and
// team owners must be full principals, so comparing them without this
// made a person fail to own the records they had just created.
func canonicalUserPrincipal(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if strings.HasPrefix(id, identity.KindUser+":") {
		return id
	}
	return identity.User(id).String()
}

// ensureOwnersTeam returns the caller's own team, creating it on first
// use with the caller as owner.
//
// A fresh node has no teams, the console's "new bot" names none, and an
// unowned team is inaccessible by design — so without this the first
// bot a person creates can never succeed. This is not the "invent a
// default team for display" the discovery rules forbid: the team is
// made because the caller asked for a bot, and it is theirs.
func (s *Server) ensureOwnersTeam(ctx context.Context, principal string) (string, error) {
	return s.consoleOperations().EnsureOwnersTeam(ctx, principal)
}
