package console

import (
	"context"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/bots"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type GroupInput struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Coordinator string  `json:"coordinator_bot_id"`
	Revision    *uint64 `json:"revision"`
}

func (s *Service) groupBotCounts(ctx context.Context) map[string]int {
	counts := map[string]int{}
	if s.cfg.Bots != nil {
		if rows, err := s.cfg.Bots.List(ctx); err == nil {
			for _, r := range rows {
				counts[GroupOfBot(r)]++
			}
		}
	}
	return counts
}
func (s *Service) Groups(ctx context.Context, claims *types.Claims) ([]GroupView, error) {
	owner, err := caller(ctx, claims)
	if err != nil {
		return nil, err
	}
	if s.cfg.Groups == nil {
		return nil, ErrUnavailable
	}
	rows, err := s.cfg.Groups.List(ctx)
	if err != nil {
		return nil, err
	}
	counts := s.groupBotCounts(ctx)
	out := []GroupView{}
	for _, r := range rows {
		if bots.MayModifyGroup(r, owner) {
			out = append(out, GroupViewOf(r, counts[r.Id], owner))
		}
	}
	return out, nil
}
func (s *Service) Group(ctx context.Context, claims *types.Claims, id string) (GroupView, error) {
	rec, err := s.ownedGroup(ctx, claims, id)
	if err != nil {
		return GroupView{}, err
	}
	return GroupViewOf(rec, s.groupBotCounts(ctx)[rec.Id], Principal(claims.UserID)), nil
}
func (s *Service) CreateGroup(ctx context.Context, claims *types.Claims, body GroupInput) (GroupView, error) {
	owner, err := caller(ctx, claims)
	if err != nil {
		return GroupView{}, err
	}
	if s.cfg.Groups == nil {
		return GroupView{}, ErrUnavailable
	}
	rec, err := s.cfg.Groups.Put(ctx, &pb.GroupRecord{Id: body.ID, Name: body.Name, Description: body.Description, CoordinatorBotId: body.Coordinator, Owner: owner}, 0)
	if err != nil {
		return GroupView{}, err
	}
	s.audit(claims, "group:create", rec.Id, rec.Name)
	return GroupViewOf(rec, 0, owner), nil
}
func (s *Service) UpdateGroup(ctx context.Context, claims *types.Claims, id string, body GroupInput) (GroupView, error) {
	cur, err := s.ownedGroup(ctx, claims, id)
	if err != nil {
		return GroupView{}, err
	}
	rev := cur.Revision
	if body.Revision != nil {
		rev = *body.Revision
	}
	rec, err := s.cfg.Groups.Put(ctx, &pb.GroupRecord{Id: id, Name: firstNonEmpty(body.Name, cur.Name), Description: firstNonEmpty(body.Description, cur.Description), CoordinatorBotId: firstNonEmpty(body.Coordinator, cur.CoordinatorBotId)}, rev)
	if err != nil {
		return GroupView{}, err
	}
	s.audit(claims, "group:update", id, rec.Name)
	return GroupViewOf(rec, s.groupBotCounts(ctx)[rec.Id], Principal(claims.UserID)), nil
}
func (s *Service) DeleteGroup(ctx context.Context, claims *types.Claims, id string) error {
	rec, err := s.ownedGroup(ctx, claims, id)
	if err != nil {
		return err
	}
	if err = s.cfg.Groups.Delete(ctx, id); err != nil {
		return err
	}
	s.audit(claims, "group:delete", id, rec.Name)
	return nil
}
func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
