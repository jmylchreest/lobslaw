package gateway

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func readerHandler(allowed ...string) *SlackHandler {
	return &SlackHandler{
		cfg: SlackConfig{AllowedChannels: allowed},
		log: discardLogger(),
		// Pointed at a closed port: every test below must refuse
		// BEFORE any network call, so reaching the API is itself a
		// failure.
		api: newSlackAPI("xoxb-test", "http://127.0.0.1:1", nil),
	}
}

// The allowlist has to bite at the tool boundary, not only on inbound
// events. Otherwise it governs what the agent HEARS while leaving what
// it can go and FETCH wide open.
func TestSlackReadRefusesChannelOutsideAllowlist(t *testing.T) {
	t.Parallel()

	h := readerHandler("C0ALLOWED")
	_, err := h.ReadConversation(context.Background(), "C0SECRET", 10)
	if !errors.Is(err, ErrSlackChannelNotAllowed) {
		t.Fatalf("err = %v, want ErrSlackChannelNotAllowed", err)
	}
	if _, err := h.ReadThread(context.Background(), "C0SECRET", "1.1", 10); !errors.Is(err, ErrSlackChannelNotAllowed) {
		t.Fatalf("ReadThread err = %v, want ErrSlackChannelNotAllowed", err)
	}
	if _, err := h.SearchConversations(context.Background(), "q", []string{"C0SECRET"}, 5); !errors.Is(err, ErrSlackChannelNotAllowed) {
		t.Fatalf("Search err = %v, want ErrSlackChannelNotAllowed", err)
	}
}

// An empty allowlist is closed here too, matching the event path.
func TestSlackReadEmptyAllowlistRefusesEverything(t *testing.T) {
	t.Parallel()

	h := readerHandler()
	if _, err := h.ReadConversation(context.Background(), "C0ANY", 10); !errors.Is(err, ErrSlackChannelNotAllowed) {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

// A wildcard allowlist says the bot may act wherever it is invited. It
// does not say one tool call may walk the whole workspace, and an
// unbounded fan-out is exactly what a confused model would ask for.
func TestSlackSearchRefusesUnboundedWildcardFanout(t *testing.T) {
	t.Parallel()

	h := readerHandler("*")
	_, err := h.SearchConversations(context.Background(), "anything", nil, 5)
	if err == nil {
		t.Fatal("a wildcard allowlist allowed a search with no named channels")
	}
	if !strings.Contains(err.Error(), "must name the conversations") {
		t.Errorf("err = %v, want an explanation of what to do instead", err)
	}
}

// With explicit channels a nil refs list is well defined: search them.
func TestSlackSearchTargetsFallBackToAllowlist(t *testing.T) {
	t.Parallel()

	h := readerHandler("C0ONE", "C0TWO")
	got, err := h.searchTargets(context.Background(), nil)
	if err != nil {
		t.Fatalf("searchTargets: %v", err)
	}
	if len(got) != 2 || got[0] != "C0ONE" || got[1] != "C0TWO" {
		t.Fatalf("targets = %v, want the allowlist", got)
	}
}

func TestSlackSearchRequiresAQuery(t *testing.T) {
	t.Parallel()

	h := readerHandler("C0ONE")
	if _, err := h.SearchConversations(context.Background(), "   ", nil, 5); err == nil {
		t.Fatal("a blank query was accepted")
	}
}

// Ids are used as-is; anything else is a name needing resolution. The
// distinction decides whether a lookup happens at all.
func TestLooksLikeChannelID(t *testing.T) {
	t.Parallel()

	for _, id := range []string{"C0ALLOWED", "D0BPM5D6QA3", "G0PRIVATE"} {
		if !looksLikeChannelID(id) {
			t.Errorf("%q not recognised as an id", id)
		}
	}
	for _, name := range []string{"general", "#general", "General", "c", ""} {
		if looksLikeChannelID(name) {
			t.Errorf("%q wrongly treated as an id", name)
		}
	}
}

// Reading history is not the event path. This filter used to drop every
// message with a bot_id — right for our own replies, wrong for every
// other bot, and an alerts channel is nothing BUT other bots. The one
// kind of channel somebody most wants summarised read as empty.
func TestIsReadableMessage(t *testing.T) {
	t.Parallel()

	readable := []slackMessage{
		{Text: "hello"},
		// The case that motivated the fix.
		{BotID: "B1", Username: "Mozart Bot", Text: "CRITICAL: pve-01 disk 94%"},
		// And the harder one: alerting webhooks post empty text and put
		// everything in the attachment.
		{BotID: "B1", Username: "Mozart Bot", Attachments: []slackAttachment{
			{Title: "Proxmox alert", Text: "storage local-lvm above threshold"},
		}},
		{BotID: "B1", Attachments: []slackAttachment{
			{Fields: []slackAttachField{{Title: "host", Value: "pve-01"}}},
		}},
		// Fallback only, which is what a terse integration sends.
		{BotID: "B1", Attachments: []slackAttachment{{Fallback: "pve-01 unreachable"}}},
	}
	for _, m := range readable {
		if !isReadableMessage(m) {
			t.Errorf("%+v was filtered out", m)
		}
	}

	unreadable := []slackMessage{
		{Text: "joined", Subtype: "channel_join"},
		{Text: "left", Subtype: "channel_leave"},
		{Text: "   "},
		{},
		// An attachment with no content anywhere is still nothing.
		{BotID: "B1", Attachments: []slackAttachment{{}}},
	}
	for _, m := range unreadable {
		if isReadableMessage(m) {
			t.Errorf("%+v was treated as readable", m)
		}
	}
}

func TestMessageTextReadsAttachments(t *testing.T) {
	t.Parallel()

	// Plain text wins outright.
	if got := messageText(slackMessage{Text: "hi", Attachments: []slackAttachment{{Text: "ignored"}}}); got != "hi" {
		t.Errorf("got %q, want the top-level text", got)
	}

	got := messageText(slackMessage{Attachments: []slackAttachment{{
		Pretext: "Alert",
		Title:   "Proxmox",
		Text:    "disk above 90%",
		Fields:  []slackAttachField{{Title: "host", Value: "pve-01"}},
	}}})
	for _, want := range []string{"Alert", "Proxmox", "disk above 90%", "host: pve-01"} {
		if !strings.Contains(got, want) {
			t.Errorf("extracted text %q is missing %q", got, want)
		}
	}
	// Fallback is a flattened copy of the same content, so it must not
	// be added on top of the structured parts.
	withBoth := messageText(slackMessage{Attachments: []slackAttachment{{
		Text: "the real text", Fallback: "the real text",
	}}})
	if strings.Count(withBoth, "the real text") != 1 {
		t.Errorf("fallback duplicated the content: %q", withBoth)
	}
}

// An alert usually has no user id, and "who raised this" is the first
// thing somebody asks about one.
func TestMessageAuthor(t *testing.T) {
	t.Parallel()

	if got := messageAuthor(slackMessage{User: "U1", Username: "ignored"}); got != "U1" {
		t.Errorf("got %q, want the user id", got)
	}
	if got := messageAuthor(slackMessage{BotID: "B1", Username: "Mozart Bot"}); got != "Mozart Bot" {
		t.Errorf("got %q, want the bot's display name", got)
	}
	if got := messageAuthor(slackMessage{BotID: "B1"}); got != "bot:B1" {
		t.Errorf("got %q, want a bot id fallback", got)
	}
	if got := messageAuthor(slackMessage{}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestReverseOrdersOldestFirst(t *testing.T) {
	t.Parallel()

	// conversations.history returns newest-first; the agent reads a
	// transcript, which only makes sense oldest-first.
	msgs := []SlackTranscriptMessage{{TS: "3"}, {TS: "2"}, {TS: "1"}}
	slices.Reverse(msgs)
	if msgs[0].TS != "1" || msgs[2].TS != "3" {
		t.Fatalf("order = %v", []string{msgs[0].TS, msgs[1].TS, msgs[2].TS})
	}
}
