package gateway

import (
	"context"
	"errors"
	"strings"
	"time"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func (s *Server) validateNotificationBot(ctx context.Context, owner, botID string) error {
	user, ok := s.enrolledUser(ctx, strings.TrimPrefix(owner, "user:"))
	if !ok {
		return errors.New("notification owner is no longer enrolled")
	}
	if s.cfg.RemoteConsole != nil {
		_, err := s.cfg.RemoteConsole.QueryConsole(ctx, &pb.QueryConsoleRequest{Identity: consoleIdentity(&types.Claims{UserID: user.ID, Roles: user.Roles, Scope: s.cfg.DefaultScope}), Query: &pb.QueryConsoleRequest_Bot{Bot: &pb.ConsoleTarget{Id: botID}}})
		return err
	}
	if s.cfg.Bots == nil {
		return errors.New("bot registry unavailable")
	}
	bot, err := s.cfg.Bots.Get(ctx, botID)
	if err != nil || bot.Owner != owner {
		return errors.New("notification agent is unavailable or no longer belongs to this owner")
	}
	return nil
}

func (s *Server) runTelegramNotifications(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		leg, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := s.cfg.Telegram.DispatchNotificationPages(leg, s.notificationEventPage)
		cancel()
		if err != nil && ctx.Err() == nil {
			s.log.Warn("telegram: task notification delivery deferred", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
