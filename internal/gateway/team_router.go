package gateway

import "context"

// TeamRouter picks which bot answers a channel message.
//
// Explicit bindings or that user's own coordinator. A missing
// binding must not route into another person's team.
type TeamRouter interface {
	BotForChannel(ctx context.Context, channel, address, userID string) string
}

func resolveTeamBot(ctx context.Context, r TeamRouter, channel, address, userID string) string {
	if r == nil {
		return ""
	}
	return r.BotForChannel(ctx, channel, address, userID)
}
