package node

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/notify"
	"github.com/jmylchreest/lobslaw/internal/scheduler"
	"github.com/jmylchreest/lobslaw/internal/tools"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// A watch is a commitment that re-arms itself.
//
// The loop is the one generation polling already uses: registered
// Idempotent(), so the handler runs BEFORE completion, and returning a
// *scheduler.RetryAfter leaves the commitment pending with a new DueAt
// and no claim. What a watch adds is state — and the state rides back
// on the SAME record the re-arm writes.
//
// That last part is load-bearing and easy to break by accident.
// rearmCommitment clones the commitment it was handed and applies the
// clone under a CAS on the revision it read, so a handler that wrote
// the record itself first would move the revision and lose its own
// re-arm to the conflict. Mutating c.Watch in place and letting the
// re-arm carry it is one Raft write rather than two, and it cannot
// race with itself.

// runWatchAsAgentTurn performs one check.
//
// Returns *scheduler.RetryAfter to keep watching, and nil when the
// watch is over — expired, or suspended after too many silent probes.
// A nil return is what lets the scheduler mark the commitment done and
// take it out of the due set for good.
func (n *Node) runWatchAsAgentTurn(ctx context.Context, c *lobslawv1.AgentCommitment) error {
	what := strings.TrimSpace(c.Params["what"])
	if what == "" {
		return fmt.Errorf("watch %q: params.what missing", c.Id)
	}
	if c.Watch == nil {
		// Without state there is nothing to compare against, and
		// treating that as a first observation would quietly restart a
		// watch whose configuration was lost.
		return fmt.Errorf("watch %q: no watch state on the record", c.Id)
	}
	st := c.Watch
	now := time.Now()

	if d, expired := decideWatchExpiry(st, what, now); expired {
		return n.finishWatch(ctx, c, d)
	}

	budget, err := compute.NewTurnBudget(compute.FromComputeConfig(n.cfg.Compute))
	if err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	probeCtx, collector := compute.WithWatchCollector(ctx)
	req := compute.ProcessMessageRequest{
		Message:   watchProbePrompt(what, st),
		Claims:    n.schedulerClaims(c.CreatedFor),
		TurnID:    fmt.Sprintf("watch-%s-%d", c.Id, now.UnixNano()),
		Budget:    budget,
		Channel:   c.Params["channel"],
		ChannelID: c.Params["chat_id"],
		Tools:     buildWatchToolList(n.toolRegistry),
	}
	resp, runErr := n.agent.RunToolCallLoop(probeCtx, req)

	// The collector is read BEFORE the error is acted on. A turn that
	// reported and then failed — a budget cap on a later call, a
	// provider dropping mid-cleanup — has already produced the only
	// thing a check exists to produce. Discarding it would throw away a
	// good observation AND advance the failure count toward suspending
	// a watch that is working.
	report, calls := collector.Report()
	if report == nil {
		return n.finishWatch(ctx, c, decideWatchFailure(st, what, probeFailureReason(resp, runErr), now))
	}
	if runErr != nil {
		n.log.Warn("watch: probe reported, then the turn failed; keeping the report",
			"watch", c.Id, "turn", req.TurnID, "err", runErr)
	}
	if calls > 1 {
		n.log.Info("watch: probe reported more than once; keeping the last",
			"watch", c.Id, "calls", calls)
	}

	d := decideWatchResult(st, what, report, now)
	n.log.Debug("watch: checked",
		"watch", c.Id, "action", d.Action.String(),
		"unchanged_runs", st.UnchangedRuns, "next", d.Retry)
	return n.finishWatch(ctx, c, d)
}

// probeFailureReason says why a check produced nothing, in words the
// user will read when the watch eventually suspends.
//
// The confirmation case is called out separately because "it reported
// no state" would be actively misleading: the check did not fail, it
// stopped to ask permission, and a scheduled turn has nobody to ask.
// That is a configuration problem with a specific fix, and the message
// is the only place it can surface.
func probeFailureReason(resp *compute.ProcessMessageResponse, runErr error) string {
	switch {
	case runErr != nil:
		return "the check could not run"
	case resp != nil && resp.NeedsConfirmation:
		return "the check needed permission to continue, and a scheduled check has nobody to ask"
	default:
		return "the check ran but reported no state"
	}
}

// finishWatch delivers whatever the decision asked for and tells the
// scheduler what to do with the commitment.
func (n *Node) finishWatch(ctx context.Context, c *lobslawv1.AgentCommitment, d watchDecision) error {
	if d.Message != "" {
		n.notifyWatch(ctx, c, d.Message)
	}
	if d.Retry <= 0 {
		n.log.Info("watch: finished", "watch", c.Id, "action", d.Action.String(),
			"reason", c.Watch.SuspendedReason)
		return nil
	}
	return scheduler.RetryAfterIn(d.Retry, "watch: "+d.Action.String())
}

// deniedInWatchProbe are the tools a check must not have.
//
// Two kinds, and both are correctness rather than tidiness.
//
// A probe that can call notify can message the user from inside a
// check — which is the one thing a watch promises not to do. The whole
// value of the feature is that an unchanged check is silent, and a
// model being helpful would break it in the direction nobody notices
// until they are being messaged hourly. The handler decides whether
// anything is said, from the digest, and it is the only thing that
// can.
//
// A probe that can schedule can schedule ITSELF: a check that decides
// to also watch something related creates a watch on every run, and
// the watches compound. Same reasoning as buildResearchToolList
// keeping write tools away from research workers, arrived at from the
// other direction.
var deniedInWatchProbe = map[string]bool{
	"notify":            true,
	"watch_create":      true,
	"watch_cancel":      true,
	"commitment_create": true,
	"commitment_cancel": true,
	"schedule_create":   true,
	"schedule_delete":   true,
}

// buildWatchToolList scopes a probe turn: everything the node can do,
// minus the tools that let a check speak or schedule, plus
// watch_report, which is Unlisted and so absent from the default.
func buildWatchToolList(reg *tools.Registry) []compute.Tool {
	if reg == nil {
		return nil
	}
	all := reg.LLMTools()
	out := make([]compute.Tool, 0, len(all)+1)
	for _, t := range all {
		if deniedInWatchProbe[t.Name] {
			continue
		}
		out = append(out, t)
	}
	for _, d := range reg.List() {
		if d.Name != "watch_report" {
			continue
		}
		schema := d.ParametersSchema
		if len(schema) == 0 {
			schema = []byte(`{"type":"object","properties":{}}`)
		}
		out = append(out, compute.Tool{
			Name:        d.Name,
			Description: d.Description,
			Parameters:  schema,
		})
		break
	}
	return out
}

// notifyWatch delivers what a watch has to say.
//
// Through the notify service by canonical user, for the reason
// notifyGeneration spells out at length: a watch created over REST has
// no open response to return into, and the channel it was created on
// may not be dialable at all.
func (n *Node) notifyWatch(ctx context.Context, c *lobslawv1.AgentCommitment, body string) {
	ch, id := c.Params["channel"], c.Params["chat_id"]
	if n.notifySvc != nil {
		if user := c.CreatedFor; user != "" {
			err := n.notifySvc.Send(ctx, notify.Notification{
				UserID:            user,
				Body:              body,
				OriginatorChannel: ch,
				OriginatorID:      id,
				Reason:            "watch",
			})
			if err == nil {
				return
			}
			n.log.Warn("watch: notify service delivery failed; trying the originating channel",
				"watch", c.Id, "user", user, "err", err)
		}
	}
	if ch == "" || id == "" {
		n.log.Info("watch: nowhere to deliver the result; the state is recorded but nobody was told",
			"watch", c.Id, "user", c.CreatedFor, "body", body)
		return
	}
	notifier := &researchNotifyAdapter{tg: n.telegramHandler, log: n.log}
	if err := notifier.Notify(ctx, ch, id, body); err != nil {
		n.log.Warn("watch: notify failed", "watch", c.Id, "err", err)
	}
}
