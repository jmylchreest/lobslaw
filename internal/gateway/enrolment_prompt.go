package gateway

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// Approving an operator enrolment from a chat.
//
// The request arrives from a laptop with no credential, so nothing
// about it is trusted. What makes a tap safe to act on is the pair of
// guarantees already in place: the prompt records WHO it was asked of
// and refuses anyone else, and the fingerprint in the message is the
// thing a human compares against what the laptop printed.
//
// No "always" button. An always-grant for issuing operator credentials
// would be a standing authority to admit anyone who asks, which is not
// a thing this should be able to express.

// EnrolmentRequest is what an approver is shown. A view rather than
// the stored record: the CSR is not here, because nothing a human
// reads needs several hundred bytes of DER.
type EnrolmentRequest struct {
	ID            string
	RequestedName string
	Fingerprint   string
	ExpiresAt     time.Time
}

// EnrolmentDecider applies a decision reached over a channel.
//
// Implemented at the node, which owns the signing key. The gateway
// knows that somebody tapped Approve; it does not know how to issue a
// certificate, and should not.
type EnrolmentDecider interface {
	Decide(ctx context.Context, id string, approve bool, by string) error
}

// enrolmentPromptTTL bounds how long the question waits.
//
// Shorter than the request's own expiry so the prompt closes first —
// a button that outlives the thing it decides is a button that fails
// when tapped, which teaches the approver to distrust all of them.
const enrolmentPromptTTL = 25 * time.Minute

// AskEnrolment puts an approve/deny question in front of the owner.
//
// Returns the prompt id so the caller can record the link. An error
// means nobody was asked — the request stays pending and an operator
// can still decide it from the CLI, which is why this never fails the
// submission itself.
func (h *TelegramHandler) AskEnrolment(ctx context.Context, req EnrolmentRequest) (string, error) {
	chatID, principal, ok := h.ownerChat()
	if !ok {
		return "", fmt.Errorf("no telegram user is mapped to the owner scope; nobody to ask")
	}
	if h.cfg.Prompts == nil {
		return "", fmt.Errorf("no prompt registry configured")
	}

	p, err := h.cfg.Prompts.Create(NewPrompt{
		Reason:    "operator enrolment: " + req.RequestedName,
		Channel:   "telegram",
		ChannelID: strconv.FormatInt(chatID, 10),
		TTL:       enrolmentPromptTTL,
		Enrolment: req.ID,
		RaisedFor: principal,
	})
	if err != nil {
		return "", err
	}

	h.sendEnrolmentKeyboard(chatID, p.ID, req)
	_ = ctx
	return p.ID, nil
}

// ownerChat finds the Telegram user mapped to the owner scope.
//
// A DM chat id equals the user id, which is what makes this work
// without storing a separate mapping. Returns the canonical principal
// alongside, because that is what the callback is compared against.
func (h *TelegramHandler) ownerChat() (chatID int64, principal string, ok bool) {
	for id, scope := range h.cfg.UserIDScopes {
		if scope != "owner" {
			continue
		}
		// principalFor needs a user; the id is all we have and all it
		// needs when no identity resolver is wired.
		return id, h.principalFor(context.Background(), &tgUser{ID: id}), true
	}
	return 0, "", false
}

// sendEnrolmentKeyboard renders the question.
//
// The fingerprint is on its own line and unadorned, because it is the
// one thing that has to survive being read aloud down a phone.
func (h *TelegramHandler) sendEnrolmentKeyboard(chatID int64, promptID string, req EnrolmentRequest) {
	text := fmt.Sprintf(
		"A laptop is asking for an operator credential.\n\n"+
			"Name requested: %s\n\n"+
			"Fingerprint:\n%s\n\n"+
			"Approve ONLY if that matches what they read you. "+
			"An operator credential can administer this cluster.",
		req.RequestedName, req.Fingerprint)
	if !req.ExpiresAt.IsZero() {
		text += fmt.Sprintf("\n\nExpires %s.", req.ExpiresAt.Format(time.RFC3339))
	}

	h.postJSON("sendMessage", map[string]any{
		"chat_id": chatID,
		"text":    text,
		"reply_markup": map[string]any{
			"inline_keyboard": [][]map[string]string{{
				{"text": "Approve", "callback_data": "prompt:approve:" + promptID},
				{"text": "Deny", "callback_data": "prompt:deny:" + promptID},
			}},
		},
	})
}

// applyEnrolmentDecision turns a resolved prompt into an issued or
// refused credential.
//
// Called after the prompt resolves, so the audience check has already
// run: the person who tapped is the person the question was asked of.
func (h *TelegramHandler) applyEnrolmentDecision(ctx context.Context, p *Prompt, approved bool, by string) {
	if p == nil || p.Enrolment == "" || h.cfg.Enrolments == nil {
		return
	}
	if err := h.cfg.Enrolments.Decide(ctx, p.Enrolment, approved, by); err != nil {
		// Surfaced to the approver, not just logged. They tapped
		// Approve and are entitled to know the certificate was not
		// issued — otherwise they tell somebody to collect a
		// credential that does not exist.
		h.log.Error("telegram: enrolment decision failed",
			"enrolment", p.Enrolment, "approved", approved, "err", err)
		h.sendText(chatIDOf(p), "That decision could not be applied: "+err.Error())
		return
	}
	if approved {
		h.sendText(chatIDOf(p), "Operator credential issued. They can collect it with `lobslaw enrol status`.")
		return
	}
	h.sendText(chatIDOf(p), "Enrolment refused.")
}

func chatIDOf(p *Prompt) int64 {
	id, err := strconv.ParseInt(p.ChannelID, 10, 64)
	if err != nil {
		return 0
	}
	return id
}
