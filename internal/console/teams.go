package console

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/bots"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func (s *Service) ensureOwnersTeam(ctx context.Context, principal string) (string, error) {
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
		// A global default slug belongs to the first user who created it.
		// A stable per-owner slug also makes concurrent first use converge.
		id := fmt.Sprintf("team-%x", sha256.Sum256([]byte(principal)))[:teamIDLength]
		team, err = s.cfg.Groups.Put(ctx, &lobslawv1.GroupRecord{
			Id:        id,
			Name:      "Your team",
			IsDefault: true,
			Owner:     principal,
			CreatedBy: principal,
		}, 0)
		if err != nil {
			team, err = s.cfg.Groups.Get(ctx, id)
			if err != nil || !bots.MayModifyGroup(team, principal) {
				return "", errors.New("could not establish the owner's team")
			}
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
// This may run during a roster read. It must never establish ownership;
// only explicitly owned bots may be attached to the owner's team.
func (s *Service) adoptOrphanBots(ctx context.Context, principal, teamID string) {
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
		if owner != principal {
			continue
		}
		b.GroupId = teamID
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
func (s *Service) ensureTeamCoordinator(ctx context.Context, principal string, team *lobslawv1.GroupRecord) error {
	if team == nil {
		return nil
	}
	if s.cfg.Bots == nil {
		return errors.New("this node does not host the bot registry")
	}
	coordID := strings.TrimSpace(team.GetCoordinatorBotId())
	if coordID == "" {
		coordID = bots.ChiefBotID
	}
	rec, err := s.cfg.Bots.Get(ctx, coordID)
	if err == nil && rec != nil {
		owner := strings.TrimSpace(rec.GetOwner())
		if owner != principal {
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

// Leave room for the coordinator's "-lead" suffix within the bot ID limit.
const teamIDLength = 45

// syncCoordinator keeps the coordinator able to reach every bot in its
// team: its may_message list is the team's roster.
//
// One direction only. The edge list is validated as a DAG on write, so
// a mutual edge would be rejected — the coordinator manages the bots,
// not the other way round.
func (s *Service) syncCoordinator(ctx context.Context, coord *lobslawv1.BotRecord) error {
	if coord == nil {
		return nil
	}
	bots, err := s.cfg.Bots.List(ctx)
	if err != nil {
		return err
	}
	want := make([]string, 0, len(bots))
	for _, b := range bots {
		if b.GetOwner() != coord.GetOwner() || b.GetOwner() == "" {
			continue
		}
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
