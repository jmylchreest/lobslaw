package gateway

import "errors"

// What a confirmation tap means, decided once for every channel.
//
// Slack and Telegram each carried the same four-verb switch: the same
// decisions, the same scopes, the same downgrade to a one-shot when a
// grant could not be recorded, and the same replies. Then the same
// mapping from a failed Resolve to what the user is told.
//
// Duplicated DECISION logic across channels is the thing commands.go
// already exists to prevent — its own comment says the alternative is
// "three places for /new to mean three slightly different things".
// This is that, for the more consequential surface: what "approve for
// the rest of this conversation" actually grants.
//
// The two had already drifted in ordering. Slack reads the prompt
// BEFORE the switch because its session grant needs prompt.SessionID;
// Telegram reads it after. That difference is real and stays with the
// channels — what moves here is only the part that must not differ.

// promptOutcome is the resolution a verb maps to.
type promptOutcome struct {
	Decision PromptDecision
	Scope    PromptScope
	Reply    string
}

// grantFns are the two grant paths, supplied by the channel because
// each reaches them differently — Slack scopes a session grant with
// the prompt's own SessionID, Telegram with the callback's chat.
//
// Each returns a description of what was granted, or "" when nothing
// was recorded. Empty is not an error: the store may be absent, or
// the floor may have refused the operation, and in both cases the
// answer is the same — narrow to a one-shot rather than promise
// something that did not happen.
type grantFns struct {
	session func() string
	always  func() string
	// sessionRisk records "everything of this KIND is fine here".
	//
	// A separate path rather than a parameter on session, because the
	// two cover genuinely different sets and the reply has to say
	// which was given. It exists for the case session cannot help
	// with: a probing agent writes a different command every time, so
	// a grant naming one command is never matched again, while one
	// naming a tier is answered once.
	sessionRisk func() string
	// noun is what the channel calls the thing a session grant covers
	// — Slack says "conversation", Telegram says "chat". Carried
	// rather than fixed here because it is the one part of the reply
	// that SHOULD differ per channel: it is the word the user already
	// uses for where they are.
	noun string
}

// resolvePromptVerb maps a tapped verb to its outcome.
//
// ok is false for a verb nothing recognises, which the caller logs and
// drops. Not an error: an unknown verb is a malformed callback, and
// the honest response to attacker-shaped input is silence.
//
// The grant calls happen HERE rather than after Resolve, deliberately.
// Recording the grant first means a resumed turn already sees it;
// resolving first lets the resume race the grant and ask a second time
// for the same operation.
func resolvePromptVerb(verb string, grants grantFns) (promptOutcome, bool) {
	if grants.sessionRisk == nil {
		// A channel that does not offer the tier button never renders
		// the verb either, so a callback carrying it is malformed
		// input rather than a missing feature.
		grants.sessionRisk = func() string { return "" }
	}
	switch verb {
	case "approve":
		return promptOutcome{PromptApproved, PromptScopeOnce, "Approved."}, true

	case "approve-session":
		granted := grants.session()
		if granted == "" {
			// Downgraded, and the reply says only "Approved." — the
			// user must not be told the agent will stop asking when it
			// will not.
			return promptOutcome{PromptApproved, PromptScopeOnce, "Approved."}, true
		}
		return promptOutcome{PromptApproved, PromptScopeSession,
			sessionGrantReply(granted, grants.noun)}, true

	case "approve-session-risk":
		granted := grants.sessionRisk()
		if granted == "" {
			return promptOutcome{PromptApproved, PromptScopeOnce, "Approved."}, true
		}
		return promptOutcome{PromptApproved, PromptScopeSession,
			riskGrantReply(granted, grants.noun)}, true

	case "approve-always":
		granted := grants.always()
		if granted == "" {
			return promptOutcome{PromptApproved, PromptScopeOnce, "Approved."}, true
		}
		return promptOutcome{PromptApproved, PromptScopeAlways, alwaysGrantReply(granted)}, true

	case "deny":
		return promptOutcome{PromptDenied, PromptScopeOnce, "Denied."}, true
	}
	return promptOutcome{}, false
}

// resolveFailureReply is what the user is told when Resolve refuses.
//
// The three cases are distinguished because the user's next move
// differs: a prompt that expired is gone, one already resolved was
// answered (possibly by somebody else, possibly on another node), and
// anything else is ours to fix. Collapsing them into "failed" makes
// all three look like a bug in the bot.
func resolveFailureReply(err error) string {
	switch {
	case errors.Is(err, ErrPromptNotFound):
		return "That prompt no longer exists."
	case errors.Is(err, ErrPromptResolved):
		return "That prompt was already resolved."
	default:
		return "Couldn't process the response."
	}
}
