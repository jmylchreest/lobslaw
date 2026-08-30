package tools

import (
	"testing"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"
)

// Two mechanisms decide what a turn in a shared conversation may see,
// and they have to agree or the narrower one is decoration:
//
//   - passive recall, via memory.Audience (the ForConversation rule);
//   - the session_* tools, via turn.Identity.Visible.
//
// Both must grant "this conversation" and "the speaker's own", and
// neither must let a Slack channel become a way to read somebody's DM.
// This pins the second one for Slack the way the Telegram group case
// is already pinned for the first.
func TestSlackSharedChannelVisibility(t *testing.T) {
	t.Parallel()

	// Bob speaking in a Slack channel.
	speaker := turn.Identity{
		UserID:    "slack-T0-U0BOB",
		Principal: identity.User("bob"),
		Channel:   "slack",
		ChannelID: "C0GENERAL",
		Shared:    true,
	}

	// Clause 1: the conversation the turn is in, whoever opened it.
	// Alice spoke first, so the record is hers — and refusing Bob would
	// refuse him the conversation he is visibly having.
	if !sessionVisibleTo(speaker, SessionBrowseInfo{
		Channel: "slack", ChannelID: "C0GENERAL",
		Owner: identity.User("alice").String(), UserID: "slack-T0-U0ALICE",
	}) {
		t.Error("the current shared conversation was not readable by a participant")
	}

	// Clause 2: alice's DM is not bob's to read, from anywhere.
	if sessionVisibleTo(speaker, SessionBrowseInfo{
		Channel: "slack", ChannelID: "D0ALICE",
		Owner: identity.User("alice").String(), UserID: "slack-T0-U0ALICE",
	}) {
		t.Fatal("a DM transcript was readable from a shared channel")
	}

	// Bob's own DM stays readable — the rule scopes by conversation, it
	// does not disown people.
	if !sessionVisibleTo(speaker, SessionBrowseInfo{
		Channel: "slack", ChannelID: "D0BOB",
		Owner: identity.User("bob").String(), UserID: "slack-T0-U0BOB",
	}) {
		t.Error("bob could not read his own DM transcript")
	}

	// A thread is a distinct conversation, so another thread in the
	// same channel is ownership-gated like anything else.
	if sessionVisibleTo(speaker, SessionBrowseInfo{
		Channel: "slack", ChannelID: "C0GENERAL/1700000000.000100",
		Owner: identity.User("alice").String(), UserID: "slack-T0-U0ALICE",
	}) {
		t.Error("another thread's transcript was readable because it shares a channel prefix")
	}
}

// A DM turn is unchanged by any of the shared-conversation work: it
// reads its own conversation and its owner's sessions, nothing else.
func TestSlackDMVisibilityUnchanged(t *testing.T) {
	t.Parallel()

	speaker := turn.Identity{
		UserID:    "slack-T0-U0BOB",
		Principal: identity.User("bob"),
		Channel:   "slack",
		ChannelID: "D0BOB",
		Shared:    false,
	}
	if !sessionVisibleTo(speaker, SessionBrowseInfo{Channel: "slack", ChannelID: "D0BOB", Owner: "user:alice"}) {
		t.Error("the turn's own conversation was refused")
	}
	if sessionVisibleTo(speaker, SessionBrowseInfo{Channel: "slack", ChannelID: "C0GENERAL", Owner: "user:alice"}) {
		t.Error("a channel bob is not in was readable from his DM")
	}
}
