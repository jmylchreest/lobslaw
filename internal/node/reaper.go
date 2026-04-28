package node

import (
	"context"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/oauth"
)

// reapInterval bounds how often the reaper sweep runs. Picked to be
// longer than typical OAuth device-code expiries (30 min on Google,
// up to 15 min on GitHub) so terminal flows have time to settle into
// their final outcome before we GC them.
const reapInterval = 30 * time.Minute

// reapMaxAgeTerminalFlow is how long we keep a finished (complete /
// expired / denied / cancelled / error) flow visible in oauth_status
// before evicting it from the tracker. 24h gives the operator a day
// to inspect what happened without the list growing unbounded.
const reapMaxAgeTerminalFlow = 24 * time.Hour

// reapMaxAgeSyntheticCred is how long a `flow-<id>` synthetic-subject
// credential is allowed to live. These arise when /userinfo failed
// during persist (the persist callback falls back to flow-id as the
// bucket key). They're inert (no skill grants), but they accumulate
// raft state until reaped.
const reapMaxAgeSyntheticCred = 1 * time.Hour

// startReaper kicks off the per-node reaper loop. Idempotent at the
// node-level — every cluster node runs it; the leader-only check on
// each delete-side raft.Apply prevents duplicate work.
func (n *Node) startReaper(ctx context.Context) {
	if n.oauthTracker == nil && n.credentialSvc == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(reapInterval)
		defer ticker.Stop()
		// First pass on a short delay so a fresh boot has settled
		// (flow tracker is empty anyway, but this prevents a thundering
		// herd of leader-elect → reap on cluster cold start).
		select {
		case <-ctx.Done():
			return
		case <-time.After(reapInterval):
		}
		for {
			n.reapOnce(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// reapOnce performs one sweep. Public-ish (lower-case but
// reachable from tests in the same package) so timing-sensitive
// integration tests can drive it explicitly.
func (n *Node) reapOnce(ctx context.Context) {
	now := time.Now()
	if n.oauthTracker != nil {
		n.reapOAuthFlows(now)
	}
	if n.credentialSvc != nil && n.raft != nil && n.raft.IsLeader() {
		n.reapSyntheticCredentials(ctx, now)
	}
}

// reapOAuthFlows evicts terminal flows older than the threshold from
// the in-memory tracker. The tracker is per-node (not raft-replicated),
// so every node runs this independently — no leader check.
func (n *Node) reapOAuthFlows(now time.Time) {
	flows := n.oauthTracker.List()
	terminal := map[string]bool{
		"complete":  true,
		"expired":   true,
		"denied":    true,
		"cancelled": true,
		"error":     true,
	}
	reaped := 0
	for _, f := range flows {
		if !terminal[f.Outcome] {
			continue
		}
		if now.Sub(f.StartedAt) < reapMaxAgeTerminalFlow {
			continue
		}
		n.oauthTracker.Forget(f.ID)
		reaped++
	}
	if reaped > 0 {
		n.log.Info("reaper: evicted terminal oauth flows", "count", reaped)
	}
}

// reapSyntheticCredentials deletes credentials whose subject is the
// synthetic `flow-<id>` placeholder set by the persist callback when
// /userinfo failed. They're inert (no skill grants survive) and
// accumulate raft state otherwise. Leader-only — Delete goes through
// raft.Apply.
func (n *Node) reapSyntheticCredentials(ctx context.Context, now time.Time) {
	creds, err := n.credentialSvc.List(ctx)
	if err != nil {
		n.log.Warn("reaper: list credentials failed", "err", err)
		return
	}
	reaped := 0
	for _, c := range creds {
		if !strings.HasPrefix(c.Subject, "flow-") {
			continue
		}
		age := now.Sub(c.CreatedAt)
		if age < reapMaxAgeSyntheticCred {
			continue
		}
		if err := n.credentialSvc.Delete(ctx, c.Provider, c.Subject); err != nil {
			n.log.Warn("reaper: delete synthetic credential failed",
				"provider", c.Provider, "subject", c.Subject, "err", err)
			continue
		}
		reaped++
	}
	if reaped > 0 {
		n.log.Info("reaper: deleted synthetic credentials", "count", reaped)
	}
}

// Compile-time guard — keeps the import alive for type signatures
// even when reaper helpers don't directly reference oauth types.
var _ = oauth.NewTracker
