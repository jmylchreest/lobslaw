package console

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/bots"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type BotPatch struct {
	Revision     *uint64   `json:"revision"`
	DisplayName  *string   `json:"display_name"`
	Description  *string   `json:"description"`
	Instructions *string   `json:"instructions"`
	Tools        *[]string `json:"tools"`
	MayMessage   *[]string `json:"may_message"`
	Enabled      *bool     `json:"enabled"`
	GroupID      *string   `json:"group_id"`
}

func (s *Service) Bots(ctx context.Context, claims *types.Claims) ([]BotView, error) {
	owner, err := caller(ctx, claims)
	if err != nil {
		return nil, err
	}
	if s.cfg.Bots == nil {
		return nil, ErrUnavailable
	}
	if s.cfg.Groups != nil {
		if _, err := s.ensureOwnersTeam(ctx, owner); err != nil {
			s.log.Warn("console: ensure owner team", "err", err)
		}
	}
	records, err := s.cfg.Bots.List(ctx)
	if err != nil {
		return nil, err
	}
	out := []BotView{}
	for _, r := range records {
		if bots.MayModify(r, owner) {
			out = append(out, BotViewOf(r))
		}
	}
	return out, nil
}
func (s *Service) Bot(ctx context.Context, claims *types.Claims, id string) (BotView, error) {
	rec, err := s.ownedBot(ctx, claims, id)
	if err != nil {
		return BotView{}, err
	}
	return BotViewOf(rec), nil
}
func (s *Service) CreateBot(ctx context.Context, claims *types.Claims, body BotView) (BotView, error) {
	owner, err := caller(ctx, claims)
	if err != nil {
		return BotView{}, err
	}
	if s.cfg.Bots == nil {
		return BotView{}, ErrUnavailable
	}
	groupID := strings.TrimSpace(body.GroupID)
	if groupID == "" {
		groupID, err = s.ensureOwnersTeam(ctx, owner)
		if err != nil {
			return BotView{}, err
		}
	}
	if _, err = s.ownedGroup(ctx, claims, groupID); err != nil {
		return BotView{}, err
	}
	rec, err := s.cfg.Bots.Put(ctx, &pb.BotRecord{Id: body.ID, DisplayName: body.DisplayName, Description: body.Description, Instructions: body.Instructions, Tools: body.Tools, MayMessage: body.MayMessage, Enabled: true, GroupId: groupID, Owner: owner, CreatedBy: owner}, 0)
	if err != nil {
		return BotView{}, err
	}
	s.audit(claims, "bot:create", rec.Id, rec.DisplayName)
	return BotViewOf(rec), nil
}
func (s *Service) UpdateBot(ctx context.Context, claims *types.Claims, id string, body BotPatch) (BotView, error) {
	rec, err := s.ownedBot(ctx, claims, id)
	if err != nil {
		return BotView{}, err
	}
	if body.Revision == nil || *body.Revision != rec.Revision {
		return BotView{}, fmt.Errorf("%w: bot changed; reload before saving", ErrConflict)
	}
	current := proto.Clone(rec).(*pb.BotRecord)
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
		if current.IsCoordinator {
			return BotView{}, fmt.Errorf("%w: the coordinator belongs to its own team and cannot be moved; make another bot the coordinator there first", ErrInvalid)
		}
		if _, err = s.ownedGroup(ctx, claims, *body.GroupID); err != nil {
			return BotView{}, err
		}
		current.GroupId = *body.GroupID
	}
	updated, err := s.cfg.Bots.Put(ctx, current, *body.Revision)
	if err != nil {
		return BotView{}, err
	}
	s.audit(claims, "bot:update", id, updated.DisplayName)
	return BotViewOf(updated), nil
}
func (s *Service) DeleteBot(ctx context.Context, claims *types.Claims, id string) error {
	if _, err := s.ownedBot(ctx, claims, id); err != nil {
		return err
	}
	if err := s.cfg.Bots.Delete(ctx, id); err != nil {
		return err
	}
	s.audit(claims, "bot:delete", id, "")
	return nil
}
