package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// GroupAPI is the teams registry as the gateway needs it.
type GroupAPI interface {
	List(ctx context.Context) ([]*lobslawv1.GroupRecord, error)
	Get(ctx context.Context, id string) (*lobslawv1.GroupRecord, error)
	Put(ctx context.Context, rec *lobslawv1.GroupRecord, expectedRevision uint64) (*lobslawv1.GroupRecord, error)
	Delete(ctx context.Context, id string) error
}

type groupJSON struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Coordinator string `json:"coordinator_bot_id,omitempty"`
	IsDefault   bool   `json:"is_default"`
	Owner       string `json:"owner,omitempty"`
	// Mine says whether the person asking may change this team, so the
	// console can hide controls that would only fail.
	Mine     bool   `json:"mine"`
	Revision uint64 `json:"revision"`
	// Bots is how many belong to this team, so a switcher can say
	// "Engineering · 4" without a second request per group.
	Bots int `json:"bots"`
}

func groupToJSON(rec *lobslawv1.GroupRecord, bots int, principal string) groupJSON {
	return groupJSON{
		Owner:       rec.GetOwner(),
		Mine:        groupMayModify(rec, principal),
		ID:          rec.GetId(),
		Name:        rec.GetName(),
		Description: rec.GetDescription(),
		Coordinator: rec.GetCoordinatorBotId(),
		IsDefault:   rec.GetIsDefault(),
		Revision:    rec.GetRevision(),
		Bots:        bots,
	}
}

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
		cur, err := s.cfg.Groups.Get(r.Context(), rest)
		if err != nil {
			s.groupErr(w, err)
			return
		}
		if !groupMayModify(cur, s.principalOf(r)) {
			s.jsonErr(w, http.StatusForbidden, "that team belongs to somebody else")
			return
		}
		if err := s.cfg.Groups.Delete(r.Context(), rest); err != nil {
			s.groupErr(w, err)
			return
		}
		s.auditRegistry(r, "group:delete", rest, cur.GetName())
		w.WriteHeader(http.StatusNoContent)
	default:
		s.jsonErr(w, http.StatusMethodNotAllowed, "unsupported method for this path")
	}
}

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.cfg.Groups.List(r.Context())
	if err != nil {
		s.groupErr(w, err)
		return
	}
	// One pass over the bots to count membership, rather than a query
	// per group: the switcher needs every count at once and the
	// registry is small.
	counts := map[string]int{}
	if s.cfg.Bots != nil {
		if bots, berr := s.cfg.Bots.List(r.Context()); berr == nil {
			for _, b := range bots {
				counts[groupOfBot(b)]++
			}
		}
	}
	out := make([]groupJSON, 0, len(groups))
	for _, g := range groups {
		if groupMayModify(g, s.principalOf(r)) {
			out = append(out, groupToJSON(g, counts[g.GetId()], s.principalOf(r)))
		}
	}
	respondJSON(w, http.StatusOK, map[string]any{"groups": out})
}

func (s *Server) getGroup(w http.ResponseWriter, r *http.Request, id string) {
	rec, err := s.cfg.Groups.Get(r.Context(), id)
	if err != nil {
		s.groupErr(w, err)
		return
	}
	if !groupMayModify(rec, s.principalOf(r)) {
		s.jsonErr(w, http.StatusForbidden, "that team belongs to somebody else")
		return
	}
	respondJSON(w, http.StatusOK, groupToJSON(rec, 0, s.principalOf(r)))
}

type groupBody struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Coordinator string  `json:"coordinator_bot_id"`
	Revision    *uint64 `json:"revision"`
}

func (s *Server) writeGroup(w http.ResponseWriter, r *http.Request, id string, rev uint64) {
	var body groupBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.jsonErr(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
		return
	}
	if id == "" {
		id = body.ID
	}
	rec, err := s.cfg.Groups.Put(r.Context(), &lobslawv1.GroupRecord{
		Id:               id,
		Name:             body.Name,
		Description:      body.Description,
		CoordinatorBotId: body.Coordinator,
		// Whoever creates a team owns it. From the session, never the
		// body: the browser is not the authority on who is asking.
		Owner: s.principalOf(r),
	}, rev)
	if err != nil {
		s.groupErr(w, err)
		return
	}
	s.auditRegistry(r, "group:create", id, rec.GetName())
	respondJSON(w, http.StatusCreated, groupToJSON(rec, 0, s.principalOf(r)))
}

func (s *Server) patchGroup(w http.ResponseWriter, r *http.Request, id string) {
	// Read the current record first so a PATCH that omits a field
	// leaves it alone rather than blanking it — the console's rename
	// box sends a name and nothing else.
	cur, err := s.cfg.Groups.Get(r.Context(), id)
	if err != nil {
		s.groupErr(w, err)
		return
	}
	if !groupMayModify(cur, s.principalOf(r)) {
		s.jsonErr(w, http.StatusForbidden,
			"that team belongs to somebody else")
		return
	}
	var body groupBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.jsonErr(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
		return
	}
	next := &lobslawv1.GroupRecord{
		Id:               id,
		Name:             firstNonEmpty(body.Name, cur.GetName()),
		Description:      firstNonEmpty(body.Description, cur.GetDescription()),
		CoordinatorBotId: firstNonEmpty(body.Coordinator, cur.GetCoordinatorBotId()),
	}
	rev := cur.GetRevision()
	if body.Revision != nil {
		rev = *body.Revision
	}
	rec, err := s.cfg.Groups.Put(r.Context(), next, rev)
	if err != nil {
		s.groupErr(w, err)
		return
	}
	s.auditRegistry(r, "group:update", id, rec.GetName())
	respondJSON(w, http.StatusOK, groupToJSON(rec, 0, s.principalOf(r)))
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// groupOfBot mirrors memory.GroupOf: an empty group_id means the
// default, so bots written before groups existed still appear in a
// team rather than in none.
func groupOfBot(rec *lobslawv1.BotRecord) string {
	if rec == nil {
		return ""
	}
	return strings.TrimSpace(rec.GetGroupId())
}

// defaultGroupID is the id of the caller's own starting team.
const defaultGroupID = memory.DefaultGroupID

func (s *Server) groupErr(w http.ResponseWriter, err error) {
	switch {
	case strings.Contains(err.Error(), "groups: not found"):
		s.jsonErr(w, http.StatusNotFound, err.Error())
	case strings.Contains(err.Error(), "changed; read it again"):
		s.jsonErr(w, http.StatusConflict, err.Error())
	case strings.Contains(err.Error(), "cannot be deleted"),
		strings.Contains(err.Error(), "is required"),
		strings.Contains(err.Error(), "must be lowercase"):
		s.jsonErr(w, http.StatusBadRequest, err.Error())
	default:
		s.jsonErr(w, http.StatusInternalServerError, err.Error())
	}
}

// groupMayModify mirrors memory.MayModifyGroup, which fails closed: an
// empty principal is nobody and an empty owner is nobody, so an
// unowned team is inaccessible rather than public. Ownership must have
// been established explicitly (see ensureOwnersTeam).
func groupMayModify(rec *lobslawv1.GroupRecord, principal string) bool {
	return memory.MayModifyGroup(rec, principal)
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
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return "", errors.New("sign in before creating a bot")
	}
	if s.cfg.Groups == nil {
		return "", errors.New("this node does not host the team registry")
	}
	groups, err := s.cfg.Groups.List(ctx)
	if err != nil {
		return "", err
	}
	var team *lobslawv1.GroupRecord
	for _, g := range groups {
		if strings.TrimSpace(g.GetOwner()) == principal {
			team = g
			break
		}
	}
	if team == nil {
		team, err = s.cfg.Groups.Put(ctx, &lobslawv1.GroupRecord{
			Id:        defaultGroupID,
			Name:      "Your team",
			IsDefault: true,
			Owner:     principal,
			CreatedBy: principal,
		}, 0)
		if err != nil {
			return "", err
		}
	}
	// Bots created before they inherited a team (or by a tool that did
	// not set one) are in no team, so the roster's filter hides them.
	// Adopt the caller's own orphans before the coordinator's contact
	// list is derived, so they appear and are reachable.
	s.adoptOrphanBots(ctx, principal, team.GetId())

	if err := s.ensureTeamCoordinator(ctx, principal, team); err != nil {
		return "", err
	}
	return team.GetId(), nil
}

// adoptOrphanBots puts the caller's team-less bots into their team.
//
// Only bots that are already theirs, or that nobody owns, are touched:
// a bot owned by somebody else is left exactly where it is. An unowned
// bot becomes the caller's, which is the same rule startup adoption
// uses for the unique operator.
func (s *Server) adoptOrphanBots(ctx context.Context, principal, teamID string) {
	if s.cfg.Bots == nil || strings.TrimSpace(principal) == "" || strings.TrimSpace(teamID) == "" {
		return
	}
	bots, err := s.cfg.Bots.List(ctx)
	if err != nil {
		return
	}
	for _, b := range bots {
		if strings.TrimSpace(b.GetGroupId()) != "" {
			continue
		}
		owner := strings.TrimSpace(b.GetOwner())
		if owner != "" && owner != principal {
			continue
		}
		b.GroupId = teamID
		if owner == "" {
			b.Owner = principal
		}
		if _, err := s.cfg.Bots.Put(ctx, b, b.GetRevision()); err != nil {
			s.log.Warn("rest: adopt orphan bot", "bot", b.GetId(), "err", err)
		}
	}
}

// ensureTeamCoordinator gives a team the bot that answers for it.
//
// Every team has a coordinator — the bot a channel reaches — and a
// team without one is one nobody can message. The chief is that bot
// for the operator who already had the assistant; a team that cannot
// take the chief gets its own, named from the team.
func (s *Server) ensureTeamCoordinator(ctx context.Context, principal string, team *lobslawv1.GroupRecord) error {
	if team == nil {
		return nil
	}
	if s.cfg.Bots == nil {
		return errors.New("this node does not host the bot registry")
	}
	coordID := strings.TrimSpace(team.GetCoordinatorBotId())
	if coordID == "" {
		coordID = memory.ChiefBotID
	}
	rec, err := s.cfg.Bots.Get(ctx, coordID)
	if err == nil && rec != nil {
		owner := strings.TrimSpace(rec.GetOwner())
		if owner != "" && owner != principal {
			// The coordinator is somebody else's. This team needs its
			// own rather than borrowing another person's bot.
			coordID = team.GetId() + "-lead"
			rec = nil
		}
	} else {
		rec = nil
	}
	if rec == nil {
		rec, err = s.cfg.Bots.Put(ctx, &lobslawv1.BotRecord{
			Id:            coordID,
			DisplayName:   coordinatorName,
			Description:   "The coordinator: answers when you message and manages the team's bots.",
			Instructions:  "You are the coordinator. Answer directly when you can, hand specialist work to the right bot, and manage the team's bots when asked.",
			IsCoordinator: true,
			Enabled:       true,
			GroupId:       team.GetId(),
			Owner:         principal,
			CreatedBy:     principal,
		}, 0)
		if err != nil {
			return err
		}
	}
	if coordID != strings.TrimSpace(team.GetCoordinatorBotId()) {
		team.CoordinatorBotId = coordID
		if _, err := s.cfg.Groups.Put(ctx, team, team.GetRevision()); err != nil {
			return err
		}
	}
	return s.syncCoordinator(ctx, rec)
}

// coordinatorName is the display name a team's coordinator shows by
// default. The bot's id stays "chief" because the personality overlay
// key derives from it.
const coordinatorName = "Coordinator"

// syncCoordinator keeps the coordinator able to reach every bot in its
// team: its may_message list is the team's roster.
//
// One direction only. The edge list is validated as a DAG on write, so
// a mutual edge would be rejected — the coordinator manages the bots,
// not the other way round.
func (s *Server) syncCoordinator(ctx context.Context, coord *lobslawv1.BotRecord) error {
	if coord == nil {
		return nil
	}
	bots, err := s.cfg.Bots.List(ctx)
	if err != nil {
		return err
	}
	want := make([]string, 0, len(bots))
	for _, b := range bots {
		if b.GetId() == coord.GetId() {
			continue
		}
		if strings.TrimSpace(b.GetGroupId()) != strings.TrimSpace(coord.GetGroupId()) {
			continue
		}
		want = append(want, b.GetId())
	}
	slices.Sort(want)
	have := append([]string(nil), coord.GetMayMessage()...)
	slices.Sort(have)
	name := strings.TrimSpace(coord.GetDisplayName())
	rename := name == "" || name == "Chief"
	if !rename && slices.Equal(have, want) {
		return nil
	}
	if rename {
		coord.DisplayName = coordinatorName
	}
	coord.MayMessage = want
	_, err = s.cfg.Bots.Put(ctx, coord, coord.GetRevision())
	return err
}
