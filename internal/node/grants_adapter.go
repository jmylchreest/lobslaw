package node

import (
	"context"
	"time"

	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/memory"
)

// grantsAdapter satisfies compute.DurableGrants over the replicated
// store.
//
// Narrow on purpose: compute can record a grant and ask whether one
// exists, and nothing else. Listing, revoking and sweeping stay on the
// store, reachable from the CLI and the wiring — the turn path has no
// business revoking a grant, and an interface that let it would make
// that an accident away.
type grantsAdapter struct{ inner *memory.SessionGrantStore }

func (a grantsAdapter) Grant(ctx context.Context, sessionID, action, resource, grantedBy string) error {
	_, err := a.inner.Grant(ctx, memory.GrantRequest{
		SessionID: sessionID,
		Action:    action,
		Resource:  resource,
		GrantedBy: grantedBy,
	})
	return err
}

func (a grantsAdapter) Granted(sessionID, action, resource string) bool {
	return a.inner.Granted(sessionID, action, resource)
}

// grantsViewAdapter serves the gateway's /grants command.
//
// A second adapter rather than widening grantsAdapter: that one exists
// so the EXECUTOR can record and check a grant, and it is deliberately
// two methods wide. Reading and revoking is a different caller with a
// different reason, and folding both into one interface would hand the
// executor a revoke it has no business holding.
type grantsViewAdapter struct{ inner *memory.SessionGrantStore }

func (a grantsViewAdapter) ForSession(sessionID string) ([]gateway.GrantView, error) {
	held, err := a.inner.ForSession(sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.GrantView, 0, len(held))
	for _, g := range held {
		view := gateway.GrantView{
			ID:        g.GetId(),
			Action:    g.GetAction(),
			Resource:  g.GetResource(),
			GrantedBy: g.GetGrantedBy(),
		}
		// Rendered as a remaining duration rather than a timestamp:
		// the reader is deciding whether to revoke, and "expires in
		// 3h" answers that where an absolute time makes them do the
		// arithmetic. A negative value means the sweeper has not run
		// yet, which the renderer says out loud.
		if ts := g.GetExpiresAt(); ts != nil {
			view.ExpiresIn = time.Until(ts.AsTime())
		}
		out = append(out, view)
	}
	return out, nil
}

func (a grantsViewAdapter) RevokeSession(ctx context.Context, sessionID string) (int, error) {
	return a.inner.RevokeSession(ctx, sessionID)
}

// sessionGrantsView returns the /grants backing, or nil when this node
// has no replicated store.
func (n *Node) sessionGrantsView() gateway.SessionGrants {
	if n.sessionGrants == nil {
		return nil
	}
	return grantsViewAdapter{inner: n.sessionGrants}
}
