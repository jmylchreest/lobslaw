package node

import (
	"context"
	"errors"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/promptgen"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// inboxIdleTick is the backstop between drains when nothing wakes the
// loop.
//
// The wake channel is the real trigger — an item posted anywhere in
// the cluster fires the FSM change callback on every voter — so this
// only has to catch what a wake cannot: a claim that expired because
// the node holding it died, and an item whose retry is waiting. Every
// thirty seconds is soon enough that neither waits noticeably and rare
// enough that an idle cluster does almost nothing.
const inboxIdleTick = 30 * time.Second

const inboxExecutionTimeout time.Duration = memory.InboxClaimTTL / 2

// inboxTerminalRetention is how long a finished item stays readable.
//
// Long enough that "what did the devops bot do last week" is a
// question the GUI can answer, bounded because every item lives in
// every snapshot on every voter.
const inboxTerminalRetention = 30 * 24 * time.Hour

// runInboxDrain works the bots' queues until ctx is cancelled.
//
// A loop on every node rather than a claimed cluster-wide task,
// because exactly-once is already guaranteed one level down: each ITEM
// is taken with a revision-checked CAS, so two nodes draining at once
// is not a race, it is throughput. A scheduled task would have added a
// second claim to reason about and bounded the latency to its cron
// period — an item posted at :10 would sit until the next minute,
// which for "ask engineering and wait" is the difference between a
// queue and a delay.
func (n *Node) runInboxDrain(ctx context.Context) {
	if n.inboxSvc == nil || n.agent == nil || n.cfg.RestoreMode {
		return
	}
	n.log.Info("bots: inbox drain running")

	timer := time.NewTimer(inboxIdleTick)
	defer timer.Stop()
	for {
		n.drainBotInboxes(ctx)
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(inboxIdleTick)
		select {
		case <-ctx.Done():
			return
		case <-n.inboxWake:
		case <-timer.C:
		}
	}
}

// wakeInboxDrain nudges the loop. Non-blocking and coalescing: a burst
// of posts produces one drain, and a send that finds the buffer full
// is dropped because the pass it would have caused is already coming.
//
// Called from wakingInbox.Post after a successful write, so it must not
// block: the buffer is one slot and a full buffer means a pass is
// already pending.
func (n *Node) wakeInboxDrain() {
	select {
	case n.inboxWake <- struct{}{}:
	default:
	}
}

// drainBotInboxes works one item for each bot that has something
// waiting.
//
// ONE item per bot per pass, not the whole queue. It keeps a failure's
// blast radius to a single task, gives every item its own session for
// the GUI to deep-link to, and means priority is re-evaluated between
// items rather than frozen at the start of a long drain. Completing an
// item is itself an inbox write, so the wake it fires brings the loop
// straight back for the next one and a backlog still clears promptly.
func (n *Node) drainBotInboxes(ctx context.Context) {
	recipients, err := n.inboxSvc.Recipients(ctx)
	if err != nil {
		n.log.Warn("inbox: list recipients failed", "err", err)
		return
	}
	for _, recipient := range recipients {
		if ctx.Err() != nil {
			return
		}
		if err := n.resumeTeamTasks(ctx, recipient); err != nil {
			n.log.Warn("inbox: resume failed", "bot", recipient, "err", err)
		}
		if err := n.drainOneInboxItem(ctx, recipient); err != nil {
			// One bot's queue failing must not stop the others being
			// worked. Logged and stepped over; the item itself already
			// carries its own error for anyone looking at the queue.
			n.log.Warn("inbox: drain failed", "bot", recipient, "err", err)
		}
	}
	n.pruneInbox(ctx)
}

// pruneInbox trims finished items on the leader only.
//
// Leader-gated because it is the one part of the drain that is not
// per-item CAS'd: every node running it would have them all proposing
// deletes for the same records, which is churn rather than a
// correctness problem, but churn on every voter every thirty seconds.
func (n *Node) pruneInbox(ctx context.Context) {
	if n.raft == nil || !n.raft.IsLeader() {
		return
	}
	pruned, err := n.inboxSvc.PruneTerminal(ctx, inboxTerminalRetention)
	if err != nil {
		n.log.Warn("inbox: prune failed", "err", err)
		return
	}
	if pruned > 0 {
		n.log.Info("inbox: pruned finished items", "count", pruned)
	}
}

func (n *Node) drainOneInboxItem(ctx context.Context, recipient string) error {
	item, err := n.inboxSvc.Claim(ctx, recipient, n.cfg.NodeID)
	if err != nil {
		return err
	}
	if item == nil {
		return nil
	}

	n.log.Info("inbox: working item",
		"bot", recipient,
		"item", item.GetId(),
		"kind", memory.InboxKindName(item.GetKind()),
		"attempt", item.GetAttempts())

	profile, err := botResolverOrNil(n.botSvc).ResolveBot(ctx, recipient)
	if err != nil {
		_, resolveErr := n.inboxSvc.Resolve(ctx, recipient, item.Id, memory.InboxOutcome{ClaimRevision: item.Revision, Claimer: item.ClaimedBy, Err: err})
		return errors.Join(err, resolveErr)
	}
	if item.RequestedBy == "" || item.RequestedBy != profile.Owner {
		_, err := n.inboxSvc.Resolve(ctx, recipient, item.Id, memory.InboxOutcome{ClaimRevision: item.Revision, Claimer: item.ClaimedBy, Err: errors.New("inbox owner does not match current bot owner")})
		return err
	}
	workCtx, cancel := context.WithTimeout(ctx, inboxExecutionTimeout)
	defer cancel()
	_, runErr := n.startTeamTask(workCtx, compute.ProcessMessageRequest{
		BotID:     recipient,
		Bot:       profile,
		Claims:    turn.ClaimsFromProto(item.TaskClaims),
		Principal: identity.Bot(recipient),
		Message:   inboxPrompt(item),
	}, item)
	if runErr != nil {
		current, err := n.inboxSvc.Get(ctx, recipient, item.Id)
		if err == nil && current.TaskId == "" {
			_, err = n.inboxSvc.Resolve(ctx, recipient, item.Id, memory.InboxOutcome{ClaimRevision: item.Revision, Claimer: item.ClaimedBy, Err: runErr})
		}
		return errors.Join(runErr, err)
	}
	return runErr
}

// inboxPrompt distinguishes a task from the result of work already done:
// otherwise the recipient starts doing the work described in a result it
// merely received. The sender is stated for the same reason a person
// wants to know who asked.
func inboxPrompt(item *lobslawv1.BotInboxItem) string {
	var lead string
	switch item.GetKind() {
	case lobslawv1.InboxKind_INBOX_KIND_TASK:
		lead = "A task has been assigned to you."
	case lobslawv1.InboxKind_INBOX_KIND_QUESTION:
		lead = "You have been asked a question. Answer it."
	case lobslawv1.InboxKind_INBOX_KIND_ANSWER:
		lead = "This is the answer to a question you asked. Use it; do not answer it again."
	case lobslawv1.InboxKind_INBOX_KIND_RESULT:
		lead = "This is the outcome of work you delegated. Note it; the work is already done."
	case lobslawv1.InboxKind_INBOX_KIND_FYI:
		lead = "This is for your information. No reply is expected."
	default:
		lead = "An item is waiting in your inbox."
	}
	sender := item.GetSender()
	if sender == "" {
		sender = "unknown"
	}
	// The lead is ours and stays trusted. Everything the SENDER wrote
	// — subject and body — is wrapped, because another bot's text is
	// no more trustworthy than a fetched page.
	//
	// It was interpolated straight into the turn before, which is the
	// exact shape this repo wraps everywhere else. Combined with
	// inbox_post being seeded default-allow, a bot that had read a
	// hostile web page could put instructions in another bot's
	// trusted prompt slot and they would read as the operator's.
	// NeutraliseDelimiters is what stops the payload closing the tag
	// and writing its own.
	return lead + "\n\n" + promptgen.WrapContext([]promptgen.ContextBlock{{
		Source:  "inbox:" + sender,
		Trust:   promptgen.TrustUntrusted,
		Content: "Subject: " + item.GetSubject() + "\n\n" + item.GetBody(),
	}})
}
