package node

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/notify"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type botNotifier struct {
	n        *Node
	fallback *notify.Service
}

func (b botNotifier) Send(ctx context.Context, message notify.Notification) error {
	delivered := false
	if message.BotID != "" && b.n.botSvc != nil && b.n.inboxSvc != nil {
		bot, err := b.n.botSvc.Get(ctx, message.BotID)
		if err != nil {
			return err
		}
		owner := b.n.identityResolver().Resolve(message.UserID).String()
		if owner != bot.Owner {
			return errors.New("notify: bot may only notify its owner")
		}
		if !message.ExpiresAt.IsZero() && message.ExpiresAt.Before(time.Now()) {
			return notify.ErrExpired
		}
		if message.ExpiresAt.IsZero() {
			message.ExpiresAt = time.Now().Add(notify.DefaultTTL)
		}
		_, err = b.n.inboxSvc.Journal(ctx, &pb.BotInboxItem{Recipient: bot.Id, Sender: "notify:" + bot.Id + ":" + strconv.FormatInt(message.ExpiresAt.Unix(), 10), RequestedBy: owner, Kind: pb.InboxKind_INBOX_KIND_FYI, Subject: "Message from " + bot.DisplayName, Body: message.Body, Result: message.Body})
		if err != nil {
			return err
		}
		delivered = true
		message.UserID = strings.TrimPrefix(owner, "user:")
	}
	if b.fallback != nil {
		err := b.fallback.Send(ctx, message)
		if err != nil && !delivered {
			return err
		}
		if err == nil {
			delivered = true
		}
	}
	if !delivered {
		return notify.ErrUserUnbound
	}
	return nil
}
