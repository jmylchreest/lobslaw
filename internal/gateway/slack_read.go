package gateway

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// Reading Slack as a knowledge source.
//
// Slack's own search.messages is not available: it needs search:read,
// which is a user-token scope a bot cannot hold. So search here is
// paging conversations.history and matching locally, which is why
// every limit below is deliberate rather than generous — an unbounded
// scan is slow, costs rate limit, and lands in the model's context.

const (
	// slackReadPageSize is one conversations.history page. Slack allows
	// up to 1000; 200 keeps a single page small enough that one slow
	// channel cannot dominate a search.
	slackReadPageSize = 200

	// slackMaxReadMessages caps one slack_read_channel call. The result
	// goes into the context window, so this is a context budget as much
	// as an API one.
	slackMaxReadMessages = 200

	// slackSearchMaxPages bounds how far back a search reads per
	// channel. Three pages is ~600 messages: enough to answer "what did
	// we say about X recently", not enough to walk a year of history on
	// every tool call.
	slackSearchMaxPages = 3

	// slackReadMaxPages bounds a single conversation read the same way.
	// Higher than the search cap because this walks one conversation
	// the caller named rather than fanning out across many.
	slackReadMaxPages = 10

	// slackSearchMaxHits caps what comes back across all channels.
	slackSearchMaxHits = 25

	// slackChannelCacheTTL is how long a name→id mapping is trusted.
	// Renames are rare and a stale mapping self-corrects on the next
	// lookup; refetching the whole conversation list per tool call
	// would not.
	slackChannelCacheTTL = 10 * time.Minute
)

// ErrSlackChannelNotAllowed is returned when a tool names a
// conversation outside allowed_channels.
//
// Enforced HERE and not only on the inbound event path. The allowlist
// governs what the agent may hear; without this it would say nothing
// about what the agent may go and fetch, and a bot restricted to one
// channel could still read every conversation it had ever been
// invited to.
var ErrSlackChannelNotAllowed = errors.New("slack: conversation not in allowed_channels")

// SlackTranscriptMessage is one message as the agent sees it.
//
// Aliased to the compute type rather than redeclared: this handler is
// the implementation of compute.SlackReader, and two structurally
// identical types would not satisfy that interface. The direction is
// the constraint — gateway may import compute, never the reverse.
type SlackTranscriptMessage = compute.SlackTranscriptMessage

// channelCache memoises name→id so a model writing "#general" does not
// cost a full conversations.list walk per call.
type channelCache struct {
	mu      sync.Mutex
	byName  map[string]string
	fetched time.Time
}

// ReadConversation returns the most recent messages in a conversation,
// oldest first. ref may be a channel id or a "#name".
func (h *SlackHandler) ReadConversation(ctx context.Context, ref string, limit int) ([]SlackTranscriptMessage, error) {
	id, err := h.resolveConversation(ctx, ref)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > slackMaxReadMessages {
		limit = slackMaxReadMessages
	}

	var out []SlackTranscriptMessage
	cursor := ""
	// Page-bounded as well as result-bounded, for the same reason
	// SearchConversations is. isReadableMessage drops joins, leaves and
	// bot noise, so a channel made mostly of those satisfies neither
	// len(out) < limit nor an empty cursor, and the loop walks the
	// entire history one API page at a time.
	for page := 0; page < slackReadMaxPages && len(out) < limit; page++ {
		size := min(slackReadPageSize, limit-len(out))
		msgs, next, err := h.api.history(ctx, id, cursor, size)
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			if !isReadableMessage(m) {
				continue
			}
			out = append(out, SlackTranscriptMessage{
				Channel: id, User: messageAuthor(m), Text: messageText(m), TS: m.TS, ThreadTS: m.ThreadTS,
			})
		}
		if next == "" || len(msgs) == 0 {
			break
		}
		cursor = next
	}
	slices.Reverse(out)
	return out, nil
}

// ReadThread returns one thread, oldest first.
func (h *SlackHandler) ReadThread(ctx context.Context, ref, ts string, limit int) ([]SlackTranscriptMessage, error) {
	id, err := h.resolveConversation(ctx, ref)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > slackMaxReadMessages {
		limit = slackMaxReadMessages
	}
	msgs, err := h.api.replies(ctx, id, ts, limit)
	if err != nil {
		return nil, err
	}
	out := make([]SlackTranscriptMessage, 0, len(msgs))
	for _, m := range msgs {
		if !isReadableMessage(m) {
			continue
		}
		out = append(out, SlackTranscriptMessage{
			Channel: id, User: messageAuthor(m), Text: messageText(m), TS: m.TS, ThreadTS: m.ThreadTS,
		})
	}
	return out, nil
}

// SearchConversations matches query against recent history in the
// named conversations, or across every allowed conversation when refs
// is empty.
//
// Substring matching, case-insensitive. Not clever, and deliberately
// so: this is a bounded local scan standing in for an API we cannot
// reach, and a scoring function would imply a completeness it does not
// have. The result says which channel and when, so the agent can go
// and read the surrounding thread.
func (h *SlackHandler) SearchConversations(ctx context.Context, query string, refs []string, limit int) ([]SlackTranscriptMessage, error) {
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return nil, errors.New("slack: search query required")
	}
	if limit <= 0 || limit > slackSearchMaxHits {
		limit = slackSearchMaxHits
	}

	targets, err := h.searchTargets(ctx, refs)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, errors.New("slack: no readable conversations; allowed_channels may be empty or list only ids this bot is not in")
	}

	var hits []SlackTranscriptMessage
	for _, id := range targets {
		cursor := ""
		for page := 0; page < slackSearchMaxPages && len(hits) < limit; page++ {
			msgs, next, err := h.api.history(ctx, id, cursor, slackReadPageSize)
			if err != nil {
				// One unreadable conversation must not fail the whole
				// search — the bot is commonly in channels whose
				// history it cannot page.
				h.log.Debug("slack: search skipped a conversation", "channel", id, "err", err)
				break
			}
			for _, m := range msgs {
				// Matched against the SAME text the reader returns, not
				// m.Text — an alert's content lives in its attachment,
				// so matching the raw field would find nothing in the
				// channels this is most useful for.
				if !isReadableMessage(m) || !strings.Contains(strings.ToLower(messageText(m)), needle) {
					continue
				}
				hits = append(hits, SlackTranscriptMessage{
					Channel: id, User: messageAuthor(m), Text: messageText(m), TS: m.TS, ThreadTS: m.ThreadTS,
				})
				if len(hits) >= limit {
					break
				}
			}
			if next == "" {
				break
			}
			cursor = next
		}
	}
	return hits, nil
}

// searchTargets resolves the conversations a search will scan, with the
// allowlist applied to every one.
//
// An empty refs list means "everywhere allowed", which is only
// expressible when allowed_channels names ids. A wildcard allowlist
// with no refs is refused rather than silently walking the workspace:
// "*" says the bot may act anywhere it is invited, not that a single
// tool call may read all of it.
func (h *SlackHandler) searchTargets(ctx context.Context, refs []string) ([]string, error) {
	if len(refs) > 0 {
		out := make([]string, 0, len(refs))
		for _, r := range refs {
			id, err := h.resolveConversation(ctx, r)
			if err != nil {
				return nil, err
			}
			out = append(out, id)
		}
		return out, nil
	}
	var out []string
	for _, c := range h.cfg.AllowedChannels {
		c = strings.TrimSpace(c)
		if c == slackWildcard {
			return nil, errors.New("slack: allowed_channels is \"*\", so a search must name the conversations to read")
		}
		if c != "" {
			out = append(out, c)
		}
	}
	return out, nil
}

// resolveConversation turns a channel id or "#name" into an id, then
// applies the allowlist.
//
// Order matters: the allowlist is checked AFTER resolution, so a name
// cannot be used to reach an id the operator did not permit.
func (h *SlackHandler) resolveConversation(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", errors.New("slack: conversation required")
	}
	id := ref
	if strings.HasPrefix(ref, "#") || !looksLikeChannelID(ref) {
		resolved, err := h.channelIDForName(ctx, strings.TrimPrefix(ref, "#"))
		if err != nil {
			return "", err
		}
		id = resolved
	}
	if !h.isAllowedChannel(id) {
		return "", fmt.Errorf("%w: %s", ErrSlackChannelNotAllowed, id)
	}
	return id, nil
}

// looksLikeChannelID reports whether a reference is already an id.
// Slack ids are a kind letter followed by uppercase alphanumerics.
func looksLikeChannelID(s string) bool {
	if len(s) < 2 {
		return false
	}
	switch s[0] {
	case 'C', 'D', 'G':
	default:
		return false
	}
	return s == strings.ToUpper(s)
}

func (h *SlackHandler) channelIDForName(ctx context.Context, name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "", errors.New("slack: conversation name required")
	}

	h.channels.mu.Lock()
	fresh := time.Since(h.channels.fetched) < slackChannelCacheTTL && h.channels.byName != nil
	if fresh {
		if id, ok := h.channels.byName[name]; ok {
			h.channels.mu.Unlock()
			return id, nil
		}
	}
	h.channels.mu.Unlock()

	byName := map[string]string{}
	cursor := ""
	for page := 0; page < 10; page++ {
		convs, next, err := h.api.listConversations(ctx, cursor)
		if err != nil {
			return "", err
		}
		for _, c := range convs {
			byName[strings.ToLower(c.Name)] = c.ID
		}
		if next == "" {
			break
		}
		cursor = next
	}

	h.channels.mu.Lock()
	h.channels.byName = byName
	h.channels.fetched = time.Now()
	h.channels.mu.Unlock()

	if id, ok := byName[name]; ok {
		return id, nil
	}
	return "", fmt.Errorf("slack: no conversation named %q that this bot can see", name)
}

// noiseSubtypes are events ABOUT a channel rather than content in it.
// Everything else is kept, including subtypes carrying real text like
// file_share and message shares.
var noiseSubtypes = map[string]bool{
	"channel_join": true, "channel_leave": true,
	"group_join": true, "group_leave": true,
	"channel_topic": true, "channel_purpose": true, "channel_name": true,
	"channel_archive": true, "channel_unarchive": true,
	"bot_add": true, "bot_remove": true,
	"message_deleted": true,
}

// isReadableMessage filters history down to content worth reading.
//
// It used to drop every message with a bot_id, reasoning that bot posts
// include our own replies and feeding those back would have the agent
// reading itself. That is true of OUR bot and false of every other one
// — and an alerts channel is nothing BUT other bots. The effect was
// that the one kind of channel somebody most wants summarised read as
// empty.
//
// Reading is not the event path. The loop risk lives in wantsEvent,
// which still refuses to answer a bot; here, our own past replies are
// simply part of the conversation.
func isReadableMessage(m slackMessage) bool {
	if noiseSubtypes[m.Subtype] {
		return false
	}
	return strings.TrimSpace(messageText(m)) != ""
}

// messageText is what a message actually says.
//
// Falls through to attachments because that is where alerting webhooks
// put everything: an integration posts an empty `text` with the host,
// severity and description in an attachment, so a reader that consults
// only Text sees a column of blanks and reports the channel empty.
func messageText(m slackMessage) string {
	if t := strings.TrimSpace(m.Text); t != "" {
		return t
	}
	var parts []string
	for _, a := range m.Attachments {
		for _, s := range []string{a.Pretext, a.Title, a.Text} {
			if s = strings.TrimSpace(s); s != "" {
				parts = append(parts, s)
			}
		}
		for _, f := range a.Fields {
			title, value := strings.TrimSpace(f.Title), strings.TrimSpace(f.Value)
			switch {
			case title != "" && value != "":
				parts = append(parts, title+": "+value)
			case value != "":
				parts = append(parts, value)
			}
		}
		// Only when the structured parts gave nothing: fallback is a
		// flattened copy of the same content, so preferring it would
		// duplicate every alert.
		if len(parts) == 0 {
			if fb := strings.TrimSpace(a.Fallback); fb != "" {
				parts = append(parts, fb)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// messageAuthor names who posted, for a reader that has to attribute an
// alert. A bot usually has no user id, only a display name.
func messageAuthor(m slackMessage) string {
	if m.User != "" {
		return m.User
	}
	if m.Username != "" {
		return m.Username
	}
	if m.BotID != "" {
		return "bot:" + m.BotID
	}
	return ""
}
