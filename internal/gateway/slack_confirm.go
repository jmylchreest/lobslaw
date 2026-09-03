package gateway

import (
	"context"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/commandrisk"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/turn"
)

// sendConfirmationBlocks renders a paused turn as a Block Kit message.
//
// The Telegram equivalent's structure carries over unchanged, including
// the part that matters most: the paused turn rides on the prompt
// record rather than a map on this handler, so an approval survives a
// restart and can be answered on a different node.
//
// action_id carries "prompt:<verb>:<id>", which is how the tap is
// routed back without a side table. Slack caps action_id at 255 bytes;
// a verb plus a ULID is nowhere near it.
func (h *SlackHandler) sendConfirmationBlocks(ctx context.Context, r *slackResponder, req compute.ProcessMessageRequest, resp *compute.ProcessMessageResponse, session SessionRef) {
	channel := r.channel
	ttl := h.cfg.ConfirmationTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	p, err := h.cfg.Prompts.Create(NewPrompt{
		TurnID:       req.TurnID,
		SessionID:    session.ChannelID,
		Reason:       resp.ConfirmationReason,
		Channel:      ChannelSlack,
		ChannelID:    channel,
		TTL:          ttl,
		Action:       resp.ConfirmationAction,
		Resource:     resp.ConfirmationResource,
		Continuation: &Continuation{Request: req, Messages: resp.Messages},
		// Who may answer, captured from the turn rather than read off
		// the tap. In a Slack channel this is load-bearing in a way it
		// is not in a Telegram DM: everyone in the room can see the
		// buttons, so without it the person who approves is whoever
		// clicks first.
		RaisedFor: session.UserID,
	})
	if err != nil {
		h.log.Error("slack: prompt registration failed", "err", err)
		r.writeFinal(ctx, "Confirmation required: "+resp.ConfirmationReason, nil)
		return
	}

	buttons := []any{button("Approve", "prompt:approve:"+p.ID, "primary")}

	// "for this conversation" and "always" are offered only when a
	// policy rule asked the question. A budget confirmation is about
	// spend, so there is no operation to remember and a button that
	// silenced future budget warnings would be actively harmful.
	perCommand := resp.ConfirmationAction != "" && resp.ConfirmationResource != "" && resp.ConfirmationGrantable
	// Offered without grantability, for the reason the Telegram path
	// explains: a command with no stable form cannot be named by a
	// grant, but its tier can, and those are the commands that repeat.
	perTier := resp.ConfirmationAction != "" && riskGrantOffered(resp.ConfirmationLabels)

	if perCommand || perTier {
		subject := grantSubject(req.Claims)
		h.pendingScopeMu.Lock()
		h.pendingScope[p.ID] = scopedOperation{
			action:   resp.ConfirmationAction,
			resource: resp.ConfirmationResource,
			subject:  subject,
			labels:   resp.ConfirmationLabels,
		}
		h.pendingScopeMu.Unlock()

		if perCommand {
			buttons = append(buttons, button("Approve here", "prompt:approve-session:"+p.ID, ""))
		}
		if perTier {
			buttons = append(buttons,
				button(riskGrantLabel(resp.ConfirmationLabels), "prompt:approve-session-risk:"+p.ID, ""))
		}
		if perCommand && subject != "" && h.cfg.ApprovalRules != nil {
			buttons = append(buttons, button("Always allow", "prompt:approve-always:"+p.ID, ""))
		}
	}
	buttons = append(buttons, button("Deny", "prompt:deny:"+p.ID, "danger"))

	blocks := []any{
		map[string]any{
			"type": "section",
			"text": map[string]any{
				"type": "mrkdwn",
				"text": "*Confirmation required*\n" + resp.ConfirmationReason,
			},
		},
		map[string]any{"type": "actions", "elements": buttons},
	}

	// The plain text is the notification body and the fallback for any
	// client that cannot render blocks. Without it the prompt arrives
	// on a phone as an empty message.
	//
	// Written into the placeholder: the question replaces "working on
	// it" rather than appearing under it, so the user is not left
	// deciding whether the bot is still thinking as well as asking.
	fallback := "Confirmation required: " + resp.ConfirmationReason
	r.writeFinal(ctx, fallback, blocks)
}

func button(label, actionID, style string) map[string]any {
	b := map[string]any{
		"type":      "button",
		"text":      map[string]any{"type": "plain_text", "text": label},
		"action_id": actionID,
		"value":     actionID,
	}
	if style != "" {
		b["style"] = style
	}
	return b
}

// handleInteraction resolves a prompt from a Block Kit button tap.
//
// Mirrors the Telegram callback path, verb for verb, so the two
// channels cannot drift on what "approve for this conversation" means.
func (h *SlackHandler) handleInteraction(ctx context.Context, in slackInteraction) {
	if in.Type != "block_actions" || len(in.Actions) == 0 {
		h.log.Debug("slack: unhandled interaction", "type", in.Type)
		return
	}
	channel := in.Channel.ID
	thread := in.Message.ThreadTS
	if thread == "" {
		thread = in.Message.TS
	}
	if !h.isAllowedChannel(channel) {
		h.log.Info("slack: interaction from a channel not in allowed_channels — dropping",
			"channel", channel, "user", in.User.ID)
		return
	}

	parts := strings.SplitN(in.Actions[0].ActionID, ":", 3)
	if len(parts) != 3 || parts[0] != "prompt" {
		h.log.Debug("slack: unhandled action_id shape", "action_id", in.Actions[0].ActionID)
		return
	}
	verb, promptID := parts[1], parts[2]

	if h.cfg.Prompts == nil {
		h.log.Warn("slack: interaction arrived but no prompt registry configured")
		return
	}
	teamID := h.teamOr(in.Team.ID)
	if teamID == "" {
		teamID = in.User.TeamID
	}
	if !h.mayResolve(ctx, promptID, teamID, in.User.ID, channel, thread) {
		return
	}

	// Read before resolving: Resolve is a CAS that can lose to another
	// node, and the loser must not consume the turn it did not win.
	prompt, getErr := h.cfg.Prompts.Get(promptID)

	// The conversation a grant is scoped to comes from the PROMPT, not
	// from the tap.
	//
	// Reconstructing it from the button was wrong, and quietly: a
	// top-level channel message has no thread_ts, so the turn is scoped
	// to "C123" — but the confirmation is posted INTO a thread, so the
	// tap comes back carrying one and rebuilt "C123/1.1". The grant
	// landed under a conversation the turn was never in, and "approve
	// here" silently asked again next time.
	//
	// It is also the same argument the subject already follows: a
	// callback is attacker-shaped input, the turn that raised the
	// question is not.
	grantSession := ""
	if getErr == nil && prompt != nil {
		grantSession = prompt.SessionID
	}

	// Whatever the verb, this prompt is finished after this tap, so its
	// remembered operation goes. The grant helpers below take it first
	// when they need it; this drains the rest — a plain "approve", a
	// "deny", or a grant that could not be recorded. Without it the map
	// only ever grows, keyed by prompts that will never be tapped again.
	defer h.takePendingScope(promptID)

	outcome, ok := resolvePromptVerb(verb, grantFns{
		session:     func() string { return h.grantForSession(ctx, promptID, grantSession) },
		sessionRisk: func() string { return h.grantForRisk(ctx, promptID, grantSession) },
		always:      func() string { return h.grantAlways(ctx, promptID) },
		noun:        "conversation",
	})
	if !ok {
		h.log.Debug("slack: unknown prompt verb", "verb", verb)
		return
	}
	decision, scope, reply := outcome.Decision, outcome.Scope, outcome.Reply

	if err := h.cfg.Prompts.Resolve(promptID, decision, scope); err != nil {
		h.log.Error("slack: resolve failed", "err", err, "id", promptID)
		h.sendText(ctx, channel, thread, resolveFailureReply(err))
		return
	}
	h.sendText(ctx, channel, thread, reply)

	if decision == PromptDenied {
		return
	}
	if getErr != nil || prompt == nil || prompt.Continuation == nil {
		h.log.Warn("slack: approve with no continuation", "prompt_id", promptID, "err", getErr)
		h.sendText(ctx, channel, thread, "I've lost track of that turn — send it again.")
		return
	}
	h.resumeAfterApproval(ctx, prompt, thread)
}

// mayResolve reports whether this tap may answer this prompt.
//
// The prompt id is unguessable, but unguessable is not authorised: the
// buttons are rendered into a channel where everyone can see and click
// them. This is the Slack form of the fix in #127, and it matters more
// here — a Telegram confirmation usually lands in a DM, a Slack one
// routinely lands in a room.
//
// Fails CLOSED. A prompt with no recorded audience is one this node
// cannot attribute an answer to.
func (h *SlackHandler) mayResolve(ctx context.Context, promptID, teamID, userID, channel, thread string) bool {
	p, err := h.cfg.Prompts.Get(promptID)
	if err != nil || p == nil {
		// Expired or reaped rather than an authorisation failure;
		// Resolve reports it properly a moment later.
		return true
	}
	if p.RaisedFor == "" {
		h.log.Warn("slack: refusing a tap on a prompt with no recorded audience", "prompt", promptID)
		h.sendText(ctx, channel, thread, "This confirmation cannot be attributed to anyone; it was not applied.")
		return false
	}
	if userID == "" {
		h.log.Warn("slack: refusing an unattributed tap", "prompt", promptID)
		return false
	}
	if !h.isAudience(ctx, teamID, userID, p.RaisedFor) {
		// Logged with both principals: somebody clicking a colleague's
		// confirmation is worth seeing, and is indistinguishable from
		// an attack otherwise.
		h.log.Warn("slack: refusing a tap from somebody the question was not asked of",
			"prompt", promptID,
			"tapped_by", h.principalFor(ctx, teamID, userID),
			"raised_for", p.RaisedFor)
		h.sendText(ctx, channel, thread, "That confirmation was not for you.")
		return false
	}
	return true
}

// isAudience reports whether this user is who the question was asked
// of, comparing principal to principal so it survives an identity
// rebind. The raw channel-derived id is accepted too, for a prompt
// raised before any binding existed.
func (h *SlackHandler) isAudience(ctx context.Context, teamID, userID, raisedFor string) bool {
	if raisedFor == "" || userID == "" {
		return false
	}
	if h.principalFor(ctx, teamID, userID) == raisedFor {
		return true
	}
	return slackUserIdentity(teamID, userID) == raisedFor
}

// grantForSession records "approved for the rest of this conversation".
// Reports whether a grant was actually recorded, so the reply does not
// promise something that did not happen.
// grantForRisk records "everything of this kind is fine in this
// conversation", the Slack half of the Telegram path of the same name.
//
// Broader than grantForSession's grant and available where that one is
// not, because a command with no stable form cannot be named by a
// grant while its tier can. riskGrantOffered is what bounds it.
func (h *SlackHandler) grantForRisk(ctx context.Context, promptID, convID string) string {
	op, ok := h.takePendingScope(promptID)
	if !ok || h.cfg.Approvals == nil || convID == "" {
		return ""
	}
	// Re-checked rather than trusted from the tap: in a Slack channel
	// the buttons are visible to the room and the action_id is not a
	// secret.
	if !riskGrantOffered(op.labels) {
		h.log.Warn("slack: refusing a label grant: not every label is grantable",
			"action", op.action, "labels", op.labels)
		return ""
	}
	// One grant per label, so a conversation that has approved reads
	// and later approves writes satisfies a command carrying both —
	// without anybody having granted that exact pair.
	keys := make([]string, 0, len(op.labels))
	for _, l := range op.labels {
		key := compute.RiskGrantResource(l)
		if key == "" {
			return ""
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return ""
	}
	grantCtx := turn.WithIdentity(ctx, turn.Identity{
		Channel:   ChannelSlack,
		ChannelID: convID,
	})
	for _, key := range keys {
		if !h.cfg.Approvals.Grant(grantCtx, op.action, key) {
			h.log.Warn("slack: could not record label approval",
				"action", op.action, "label", key)
			return ""
		}
	}
	h.log.Info("slack: labels approved for this conversation",
		"action", op.action, "labels", op.labels, "conversation", convID)
	return commandrisk.RenderLabels(op.labels)
}

func (h *SlackHandler) grantForSession(ctx context.Context, promptID, convID string) string {
	// Taken before the store is checked so the entry is consumed even
	// when there is nowhere to record the grant. A pending scope that
	// outlives its prompt is a slow leak and, worse, something a later
	// tap on a recycled id could pick up.
	op, ok := h.takePendingScope(promptID)
	if !ok || h.cfg.Approvals == nil {
		return ""
	}
	if convID == "" {
		// No conversation to scope to. Refusing means the caller
		// narrows to a one-shot approval, which is the safe direction:
		// a grant scoped to nothing is either scoped to everything or
		// findable by nobody, and both are wrong.
		h.log.Warn("slack: session grant has no conversation; narrowing to once",
			"prompt", promptID)
		return ""
	}
	grantCtx := turn.WithIdentity(ctx, turn.Identity{
		Channel:   ChannelSlack,
		ChannelID: convID,
	})
	if !h.cfg.Approvals.Grant(grantCtx, op.action, op.resource) {
		h.log.Warn("slack: could not record session approval",
			"action", op.action, "resource", op.resource)
		return ""
	}
	h.log.Info("slack: approved for this conversation",
		"action", op.action, "resource", op.resource, "conversation", convID)
	return op.resource
}

// grantAlways mints the revocable policy rule behind "always".
//
// Returns the resource granted so the reply can name it, for the
// reason spelled out on the Telegram twin: a permanent grant whose
// scope the user cannot see is one they cannot audit.
func (h *SlackHandler) grantAlways(ctx context.Context, promptID string) string {
	// Taken first, for the same reason as grantForSession.
	op, ok := h.takePendingScope(promptID)
	if !ok || op.subject == "" || h.cfg.ApprovalRules == nil {
		return ""
	}
	rule, err := h.cfg.ApprovalRules.Mint(ctx, policy.MintRequest{
		PromptID: promptID,
		Subject:  op.subject,
		Action:   op.action,
		Resource: op.resource,
	})
	if err != nil {
		h.log.Warn("slack: could not mint a permanent approval",
			"action", op.action, "resource", op.resource, "err", err)
		return ""
	}
	h.log.Info("slack: permanent approval recorded",
		"rule_id", rule.Id, "subject", op.subject,
		"action", op.action, "resource", op.resource)
	return op.resource
}

func (h *SlackHandler) takePendingScope(promptID string) (scopedOperation, bool) {
	h.pendingScopeMu.Lock()
	defer h.pendingScopeMu.Unlock()
	op, ok := h.pendingScope[promptID]
	delete(h.pendingScope, promptID)
	return op, ok
}

// resumeSessionFor is the conversation a resumed leg runs in.
//
// UserID carries through, and that is the whole point of the function
// existing: the resumed leg can raise a SECOND confirmation, and
// sendConfirmationBlocks stamps RaisedFor from this field. Left empty,
// that prompt is attributable to nobody, mayResolve refuses every tap
// — including from the person it was asked of — and a turn that had
// been approved once has no ending but its TTL.
func resumeSessionFor(p *Prompt) SessionRef {
	return SessionRef{Channel: ChannelSlack, ChannelID: p.SessionID, UserID: p.RaisedFor}
}

// resumeAfterApproval re-enters the agent loop with a relaxed budget
// and delivers the result back to the thread the question was asked in.
func (h *SlackHandler) resumeAfterApproval(ctx context.Context, p *Prompt, thread string) {
	cont := p.Continuation
	channel := p.ChannelID
	session := resumeSessionFor(p)

	// Tools stay nil: fillDefaults repopulates them from the resuming
	// node's own registry, so a serialised definition cannot outlive
	// the redeploy that changed it.
	cont.Request.TurnID = p.TurnID
	cont.Request.Channel = ChannelSlack
	cont.Request.ChannelID = p.SessionID

	// The resumed leg runs the agent again and takes just as long as
	// the first one, so it gets its own placeholder. Without it the
	// user taps Approve and is back to silence — worse than the first
	// wait, because they have just been told something is happening.
	turnCtx, r, cleanup := h.startResponsivenessGuards(ctx, channel, thread, thread)
	defer cleanup()

	cont.Request.Budget.Relax()
	// The policy equivalent of Relax — see the Telegram twin. From the
	// prompt record, never the interaction payload.
	turnCtx = compute.WithTurnApproval(turnCtx, p.Action, p.Resource)
	resp, err := h.agent.ResumeFromConfirmation(turnCtx, cont.Request, cont.Messages)
	if err != nil {
		h.log.Error("slack: resume failed", "turn_id", cont.Request.TurnID, "err", err)
		r.writeFinal(ctx, classifyAgentError(err), nil)
		return
	}

	if newTurn := newTurnMessages(resp.Messages, resp.TurnStartIndex); len(newTurn) > 0 {
		h.conv.Append(ctx, session, cont.Request.TurnID, newTurn)
	}

	switch {
	case resp.NeedsConfirmation:
		h.sendConfirmationBlocks(ctx, r, cont.Request, resp, session)
	case resp.Reply == "":
		r.writeFinal(ctx, "(empty reply)", nil)
	default:
		r.writeFinal(ctx, resp.Reply, nil)
	}
	// The resumed leg is where a confirmed generation actually
	// produces its file, so this is the delivery point that matters
	// for anything gated behind an approval.
	h.SendAttachments(ctx, channel, thread, resp.Attachments, h.cfg.ArtifactOpener)
}
