package node

import (
	"context"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// wakingInbox wraps the inbox so a post nudges the drain loop.
//
// The drain polls on an idle tick, but that ticks every thirty seconds
// — an item handed to another bot at :10 would sit until :40. The wake
// is what makes it a queue rather than a delay, and it belongs on the
// one path that CREATES work.
//
// It satisfies both consumer interfaces (the gateway's and the tools')
// by embedding the service and overriding only Post.
type wakingInbox struct {
	*memory.InboxService
	wake func()
}

func (w wakingInbox) Post(ctx context.Context, item *lobslawv1.BotInboxItem) (*lobslawv1.BotInboxItem, error) {
	rec, err := w.InboxService.Post(ctx, item)
	if err == nil && w.wake != nil {
		w.wake()
	}
	return rec, err
}
