package node

import (
	"context"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/memory"
)

func (n *Node) teamRouterOrNil() gateway.TeamRouter {
	if !gateComputeTeams(n.cfg) || n.groupSvc == nil {
		return nil
	}
	return &teamRouter{groups: n.groupSvc, prefs: n.userPrefsSvc}
}

func (n *Node) teamBotsOrNil() gateway.BotAPI {
	if !gateComputeTeams(n.cfg) {
		return nil
	}
	return n.botSvc
}

func (n *Node) teamGroupsOrNil() gateway.GroupAPI {
	if !gateComputeTeams(n.cfg) {
		return nil
	}
	return n.groupSvc
}

func (n *Node) teamInboxOrNil() gateway.InboxAPI {
	if !gateComputeTeams(n.cfg) || n.inboxSvc == nil {
		return nil
	}
	// The wrapper, not the plain service: posting work must wake the
	// drain rather than wait for its idle tick.
	return n.inboxAPI
}

type teamRouter struct {
	groups *memory.GroupService
	prefs  *memory.UserPrefsService
}

func (r *teamRouter) BotForChannel(ctx context.Context, channel, address, userID string) string {
	if r == nil || r.groups == nil {
		return ""
	}
	owner := strings.TrimSpace(userID)
	if owner == "" && r.prefs != nil {
		bound, err := r.prefs.PrincipalFor(ctx, channel, address)
		if err != nil {
			return ""
		}
		owner = strings.TrimSpace(bound)
	}
	if owner == "" {
		return ""
	}
	groups, err := r.groups.List(ctx)
	if err != nil {
		return ""
	}
	fallback := ""
	for _, g := range groups {
		if strings.TrimSpace(g.GetOwner()) != owner {
			continue
		}
		coord := strings.TrimSpace(g.GetCoordinatorBotId())
		if coord == "" {
			continue
		}
		if g.GetIsDefault() {
			return coord
		}
		if fallback == "" {
			fallback = coord
		}
	}
	return fallback
}
