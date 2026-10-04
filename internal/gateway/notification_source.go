package gateway

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/bots"
	"github.com/jmylchreest/lobslaw/internal/notify"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type notificationInbox interface {
	NotificationPage(context.Context, string, string, int) ([]*pb.ConsoleInboxItem, string, error)
}
type notificationCursor struct {
	Bot   string
	After string
}

func encodeNotificationCursor(bot, after string) string {
	raw, _ := json.Marshal(notificationCursor{Bot: bot, After: after})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (s *Server) notificationBots(ctx context.Context, claims *types.Claims, owner string) ([]*pb.ConsoleBot, error) {
	var bots []*pb.ConsoleBot
	if s.cfg.RemoteConsole != nil {
		reply, err := s.cfg.RemoteConsole.QueryConsole(ctx, &pb.QueryConsoleRequest{Identity: consoleIdentity(claims), Query: &pb.QueryConsoleRequest_Bots{Bots: &pb.ConsoleEmpty{}}})
		if err != nil {
			return nil, err
		}
		bots = reply.GetBots().GetBots()
	} else if s.cfg.Bots != nil {
		records, err := s.cfg.Bots.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, bot := range records {
			if bot.Owner == owner {
				bots = append(bots, &pb.ConsoleBot{Id: bot.Id, DisplayName: bot.DisplayName})
			}
		}
	}
	slices.SortFunc(bots, func(a, b *pb.ConsoleBot) int { return cmp.Compare(a.Id, b.Id) })
	return bots, nil
}

func (s *Server) notificationEventPage(ctx context.Context, owner, after string) (notify.EventPage, error) {
	var out notify.EventPage
	user, ok := s.enrolledUser(ctx, strings.TrimPrefix(owner, "user:"))
	if !ok {
		return out, nil
	}
	claims := &types.Claims{UserID: user.ID, Roles: user.Roles, Scope: s.cfg.DefaultScope}
	var cursor notificationCursor
	if after != "" {
		if len(after) > 2048 {
			return out, errors.New("notification cursor too long")
		}
		raw, err := base64.RawURLEncoding.DecodeString(after)
		if err != nil {
			return out, err
		}
		if err := json.Unmarshal(raw, &cursor); err != nil {
			return out, err
		}
	}
	bots, err := s.notificationBots(ctx, claims, owner)
	if err != nil {
		return out, err
	}
	index := 0
	for index < len(bots) && bots[index].Id < cursor.Bot {
		index++
	}
	if index == len(bots) {
		return out, nil
	}
	bot := bots[index]
	if bot.Id != cursor.Bot {
		cursor.After = ""
	}
	page, err := s.notificationCandidates(ctx, claims, bot.Id, cursor.After, 128)
	if err != nil {
		return out, err
	}
	for _, item := range page.Items {
		if item.RequestedBy != owner {
			continue
		}
		task, err := s.notificationTask(ctx, claims, owner, item)
		if err != nil {
			out.Incomplete = true
			s.log.Warn("notification candidate deferred", "task_id", item.TaskId, "err", err)
			continue
		}
		if event, ok := inboxPushEvent(bot, item, task); ok {
			out.Events = append(out.Events, event)
		}
	}
	if page.NextCursor != "" {
		out.Next = encodeNotificationCursor(bot.Id, page.NextCursor)
	} else if index+1 < len(bots) {
		out.Next = encodeNotificationCursor(bots[index+1].Id, "")
	}
	return out, nil
}

func (s *Server) notificationCandidates(ctx context.Context, claims *types.Claims, bot, after string, limit int) (*pb.ConsoleNotificationPage, error) {
	if s.cfg.RemoteConsole != nil {
		reply, err := s.cfg.RemoteConsole.QueryConsole(ctx, &pb.QueryConsoleRequest{Identity: consoleIdentity(claims), Query: &pb.QueryConsoleRequest_NotificationCandidates{NotificationCandidates: &pb.ConsoleNotificationQuery{Bot: bot, After: after, Limit: int32(limit)}}})
		if err != nil {
			return nil, err
		}
		if reply.GetNotificationCandidates() == nil {
			return nil, errors.New("backend does not support the notification outbox")
		}
		return reply.GetNotificationCandidates(), nil
	}
	reader, ok := s.cfg.Inbox.(notificationInbox)
	if !ok {
		return nil, errors.New("notification outbox unavailable")
	}
	items, next, err := reader.NotificationPage(ctx, bot, after, limit)
	return &pb.ConsoleNotificationPage{Items: items, NextCursor: next}, err
}

func (s *Server) notificationTask(ctx context.Context, claims *types.Claims, owner string, item *pb.ConsoleInboxItem) (*pb.TaskApprovalRecord, error) {
	if item.Status != "waiting" || item.TaskId == "" {
		return nil, nil
	}
	query := &pb.GetTaskApprovalRequest{Id: item.TaskId, Owner: owner}
	if s.cfg.RemoteConsole != nil {
		reply, err := s.cfg.RemoteConsole.QueryConsole(ctx, &pb.QueryConsoleRequest{Identity: consoleIdentity(claims), Query: &pb.QueryConsoleRequest_TaskApproval{TaskApproval: query}})
		if err != nil {
			return nil, err
		}
		if reply.GetTaskApproval().GetRecord() == nil {
			return nil, errors.New("missing task evidence")
		}
		return reply.GetTaskApproval().GetRecord(), nil
	}
	if s.cfg.TaskApprovals == nil {
		return nil, errors.New("task evidence unavailable")
	}
	reply, err := s.cfg.TaskApprovals.GetTaskApproval(ctx, query)
	if err != nil {
		return nil, err
	}
	if reply.GetRecord() == nil {
		return nil, errors.New("missing task evidence")
	}
	return reply.GetRecord(), nil
}

func (s *Server) queryConsoleNotifications(ctx context.Context, in *pb.QueryConsoleRequest) (*pb.QueryConsoleResponse, error) {
	query := in.GetNotificationCandidates()
	if query.Limit < 1 || query.Limit > bots.MaxNotificationPage || len(query.After) > 512 {
		return nil, status.Error(codes.InvalidArgument, "invalid notification page")
	}
	claims, _ := ctx.Value(forwardedConsoleIdentity{}).(*types.Claims)
	if claims == nil || s.cfg.Bots == nil {
		return nil, status.Error(codes.Unavailable, "notification registry unavailable")
	}
	bot, err := s.cfg.Bots.Get(ctx, query.Bot)
	if err != nil || bot.Owner != canonicalUserPrincipal(claims.UserID) {
		return nil, status.Error(codes.NotFound, "bot not found")
	}
	page, err := s.notificationCandidates(ctx, claims, query.Bot, query.After, int(query.Limit))
	if err != nil {
		return nil, err
	}
	owner := canonicalUserPrincipal(claims.UserID)
	page.Items = slices.DeleteFunc(page.Items, func(item *pb.ConsoleInboxItem) bool { return item.RequestedBy != owner })
	return &pb.QueryConsoleResponse{Result: &pb.QueryConsoleResponse_NotificationCandidates{NotificationCandidates: page}}, nil
}
