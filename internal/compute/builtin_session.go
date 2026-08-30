package compute

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// SessionBrowser is the read side of the transcript store, as the
// agent's session tools need it. Narrow interface for the usual
// import-cycle reason; the node wires a memory.SessionService adapter.
//
// Search and Recent take a visibility predicate rather than being
// filtered by their caller afterwards: both apply a result limit
// internally, and filtering after the limit would let another user's
// conversations crowd the caller's own out of the window.
type SessionBrowser interface {
	// Search finds conversations containing text.
	Search(ctx context.Context, q SessionBrowseQuery) ([]SessionBrowseHit, error)
	// Recent lists conversations newest-first. visible, when non-nil,
	// is consulted before the limit is applied.
	Recent(ctx context.Context, limit int, visible SessionVisibleFunc) ([]SessionBrowseInfo, error)
	// Read returns a window of one conversation's transcript.
	Read(ctx context.Context, key turn.SessionKey, fromSeq uint64, limit int) ([]Message, error)
	// Info returns one conversation's index entry. found is false for
	// a conversation that doesn't exist, which is not an error.
	Info(ctx context.Context, key turn.SessionKey) (info SessionBrowseInfo, found bool, err error)
}

// SessionVisibleFunc reports whether one stored conversation may be
// shown to the caller. A nil SessionVisibleFunc means "everything is
// visible" — see sessionVisibility for when that is legitimate.
type SessionVisibleFunc func(SessionBrowseInfo) bool

// SessionBrowseQuery mirrors memory.SessionSearchQuery.
type SessionBrowseQuery struct {
	Text               string
	Channel            string
	UserID             string
	Limit              int
	SnippetsPerSession int
	// Visible gates which conversations are searched at all. Applied
	// before Limit, so a user's own results are never displaced by
	// hits they aren't allowed to see.
	Visible SessionVisibleFunc
}

// SessionBrowseInfo is a conversation's index entry.
type SessionBrowseInfo struct {
	Channel   string
	ChannelID string
	// Owner is UserID resolved to a canonical principal by the node's
	// alias map. Empty when nothing resolved it, in which case
	// visibility falls back to comparing the raw channel id.
	Owner     string
	Title     string
	UserID    string
	Messages  uint64
	UpdatedAt string
	Summary   string
}

// SessionBrowseHit is one search result.
type SessionBrowseHit struct {
	Info     SessionBrowseInfo
	Matches  int
	Snippets []SessionBrowseSnippet
}

// SessionBrowseSnippet locates a match within a transcript.
type SessionBrowseSnippet struct {
	Seq  uint64
	Role string
	Text string
}

// Visible implements the scoping rule for one stored conversation.
//
// Two clauses, and the first is the subtle one. A Telegram group chat
// is a single session shared by every member: the record's UserId is
// whoever spoke first, while ChannelID is the chat. Scoping purely on
// ownership would tell the second member to get lost in the middle of
// the conversation they are having. So the conversation the turn is
// already inside is always readable — the user can see it by scrolling
// up regardless of what we do here.
//
// Everything else is ownership-gated: another session is visible only
// when it was opened by this same user. Note this is deliberately
// narrower than "same human" — a solo operator who uses both Telegram
// and the REST API has two identities and will not find one channel's
// threads from the other. Cross-identity aliasing is a mapping we
// don't have yet, and inventing one here would mean guessing; a false
// negative costs recall, a false positive is the bug this fixes.
// A function rather than a method, now that Identity lives in its own
// package — but it reads better this way regardless. Visible takes a
// SessionBrowseInfo, so it was never a fact about the identity: it is
// the SESSION that is visible, and hanging it off Identity had the
// sentence backwards.
func sessionVisibleTo(t turn.Identity, i SessionBrowseInfo) bool {
	if identityIsCurrent(t, i.Channel, i.ChannelID) {
		return true
	}
	// Compared as principals, not as channel ids. The record stores the
	// id of whichever channel opened it, so "tg-@alice" and a REST
	// subject are different strings for the same person — matching on
	// them makes one human several, and they stop finding their own
	// history the moment they switch app. Owner is that id already
	// resolved through the operator's alias map; it falls back to the
	// raw id when no aliases are configured, which is the same
	// comparison as before for every deployment that has none.
	if i.Owner != "" && !t.Principal.IsZero() {
		return i.Owner == t.Principal.String()
	}
	return i.UserID == t.UserID
}

// isCurrent reports whether an address is the turn's own conversation.
// Both halves must be set: a scheduler turn has no channel, and an
// empty address must not match the sessions that predate the UserID
// being recorded.
func identityIsCurrent(t turn.Identity, channel, channelID string) bool {
	if t.Channel == "" || t.ChannelID == "" {
		return false
	}
	return channel == t.Channel && channelID == t.ChannelID
}

// sessionVisibility is the single place that decides what an unscoped
// context means, so the answer can't drift per tool.
//
// No scope means unscoped: every conversation is visible. That is a
// deliberate fail-open and it is only defensible because of who can
// reach it. Agent.runLoop attaches a scope unconditionally — including
// for anonymous turns, which get the empty-user scope rather than no
// scope — so nothing the model drives can arrive here without one. A
// bare context comes from operator tooling (`lobslaw session` reads
// the store directly), the compactor, or tests: callers that already hold
// the whole database and gain nothing from being refused a view of it.
//
// The corollary is that a new caller of these builtins MUST attach a
// scope. Denying instead would make that mistake loud, but it would
// also refuse the operator their own data on a single-user node, which
// is the common case; the guard is the agent-loop test that asserts
// the scope is attached, not a runtime error here.
func sessionVisibility(ctx context.Context) SessionVisibleFunc {
	identity, ok := turn.IdentityFrom(ctx)
	if !ok {
		return nil
	}
	return SessionVisibilityFor(identity)
}

// SessionVisibilityFor builds the predicate for one identity.
//
// Exported because callers outside this package construct an identity
// directly rather than pulling one off a context — the node's session
// store adapter and its tests do exactly that. Before Identity moved
// to its own package they reached for a METHOD on it, which is not a
// thing an identity should have: what is visible is the session.
func SessionVisibilityFor(t turn.Identity) SessionVisibleFunc {
	return func(i SessionBrowseInfo) bool { return sessionVisibleTo(t, i) }
}

// SessionToolConfig bounds what the session tools may return. Every
// result goes into the agent's context window, so an unbounded
// session_read would undo the context budget in a single tool call.
type SessionToolConfig struct {
	Browser SessionBrowser
	// MaxSearchResults caps conversations per search. 0 → 5.
	MaxSearchResults int
	// MaxSnippets caps snippets per conversation. 0 → 3.
	MaxSnippets int
	// MaxReadMessages caps messages per session_read. 0 → 40.
	MaxReadMessages int
}

// Session tool defaults.
const (
	DefaultMaxSearchResults = 5
	DefaultMaxSnippets      = 3
	DefaultMaxReadMessages  = 40
)

// RegisterSessionBuiltins wires session_search / session_list /
// session_read.
func RegisterSessionBuiltins(b *Builtins, cfg SessionToolConfig) error {
	if cfg.Browser == nil {
		return errors.New("session builtins: Browser required")
	}
	if cfg.MaxSearchResults <= 0 {
		cfg.MaxSearchResults = DefaultMaxSearchResults
	}
	if cfg.MaxSnippets <= 0 {
		cfg.MaxSnippets = DefaultMaxSnippets
	}
	if cfg.MaxReadMessages <= 0 {
		cfg.MaxReadMessages = DefaultMaxReadMessages
	}
	if err := b.Register("session_search", newSessionSearchHandler(cfg)); err != nil {
		return err
	}
	if err := b.Register("session_list", newSessionListHandler(cfg)); err != nil {
		return err
	}
	return b.Register("session_read", newSessionReadHandler(cfg))
}

func newSessionSearchHandler(cfg SessionToolConfig) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		query := strings.TrimSpace(args["query"])
		if query == "" {
			return nil, 2, errors.New("query is required")
		}
		limit := clampArg(args["limit"], cfg.MaxSearchResults, cfg.MaxSearchResults)
		visible := sessionVisibility(ctx)
		hits, err := cfg.Browser.Search(ctx, SessionBrowseQuery{
			Text:               query,
			Channel:            strings.TrimSpace(args["channel"]),
			Limit:              limit,
			SnippetsPerSession: cfg.MaxSnippets,
			Visible:            visible,
		})
		if err != nil {
			return nil, 1, err
		}
		hits = filterHits(hits, visible)
		if len(hits) == 0 {
			return []byte(fmt.Sprintf("No past conversation contains %q.", query)), 0, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d conversation(s) mention %q:\n", len(hits), query)
		for _, h := range hits {
			fmt.Fprintf(&b, "\n%s (%d match(es), last active %s)\n",
				describeSession(h.Info), h.Matches, h.Info.UpdatedAt)
			for _, s := range h.Snippets {
				fmt.Fprintf(&b, "  [#%d %s] %s\n", s.Seq, s.Role, collapseWhitespace(s.Text))
			}
		}
		b.WriteString("\nUse session_read with the channel and channel_id to see more.")
		return []byte(b.String()), 0, nil
	}
}

func newSessionListHandler(cfg SessionToolConfig) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		limit := clampArg(args["limit"], 10, 50)
		visible := sessionVisibility(ctx)
		infos, err := cfg.Browser.Recent(ctx, limit, visible)
		if err != nil {
			return nil, 1, err
		}
		infos = filterInfos(infos, visible)
		if len(infos) == 0 {
			return []byte("No stored conversations yet."), 0, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d conversation(s), most recent first:\n", len(infos))
		for _, i := range infos {
			fmt.Fprintf(&b, "  %s — %d messages, last active %s\n",
				describeSession(i), i.Messages, i.UpdatedAt)
		}
		return []byte(b.String()), 0, nil
	}
}

func newSessionReadHandler(cfg SessionToolConfig) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		channel := strings.TrimSpace(args["channel"])
		channelID := strings.TrimSpace(args["channel_id"])
		if channel == "" || channelID == "" {
			return nil, 2, errors.New("channel and channel_id are required (both come from session_search or session_list)")
		}
		var fromSeq uint64
		if v := strings.TrimSpace(args["from_seq"]); v != "" {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return nil, 2, fmt.Errorf("from_seq must be a number: %w", err)
			}
			fromSeq = n
		}
		limit := clampArg(args["limit"], cfg.MaxReadMessages, cfg.MaxReadMessages)
		key := turn.SessionKey{Channel: channel, ChannelID: channelID}
		if err := authorizeSessionRead(ctx, cfg.Browser, key); err != nil {
			return nil, 1, err
		}
		msgs, err := cfg.Browser.Read(ctx, key, fromSeq, limit)
		if err != nil {
			return nil, 1, err
		}
		if len(msgs) == 0 {
			return []byte("That conversation has no messages in that range."), 0, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s:%s, %d message(s) from #%d:\n", channel, channelID, len(msgs), fromSeq)
		for _, m := range msgs {
			b.WriteString(renderForSummary(m, DefaultCompactToolResultBytes))
		}
		return []byte(b.String()), 0, nil
	}
}

// errSessionNotVisible is what session_read returns for a
// conversation the turn may not see. Identical to the wording for a
// conversation that doesn't exist, deliberately: distinguishing the
// two would turn the tool into an oracle for "does user X have a
// thread with chat id Y", which is most of what the ids leak anyway.
var errSessionNotVisible = errors.New("no conversation with that channel and channel_id is available")

// authorizeSessionRead gates session_read on the turn's scope. The
// current conversation short-circuits without touching the store —
// it's readable by definition, and a brand-new session has no index
// record yet.
func authorizeSessionRead(ctx context.Context, browser SessionBrowser, key turn.SessionKey) error {
	identity, ok := turn.IdentityFrom(ctx)
	if !ok {
		return nil
	}
	if identityIsCurrent(identity, key.Channel, key.ChannelID) {
		return nil
	}
	info, found, err := browser.Info(ctx, key)
	if err != nil {
		return err
	}
	if !found || !sessionVisibleTo(identity, info) {
		return errSessionNotVisible
	}
	return nil
}

// filterHits and filterInfos re-apply the visibility predicate to
// whatever the browser returned. The browser is asked to filter (it
// has to, or the limit would be applied to the wrong set) and the
// wired implementation does; re-checking here means a browser that
// quietly ignores the predicate degrades into empty results rather
// than into the leak this scoping exists to close.
func filterHits(hits []SessionBrowseHit, visible SessionVisibleFunc) []SessionBrowseHit {
	if visible == nil {
		return hits
	}
	// Copies rather than filtering in place: the browser may be
	// handing back a slice it still owns.
	out := make([]SessionBrowseHit, 0, len(hits))
	for _, h := range hits {
		if visible(h.Info) {
			out = append(out, h)
		}
	}
	return out
}

func filterInfos(infos []SessionBrowseInfo, visible SessionVisibleFunc) []SessionBrowseInfo {
	if visible == nil {
		return infos
	}
	out := make([]SessionBrowseInfo, 0, len(infos))
	for _, i := range infos {
		if visible(i) {
			out = append(out, i)
		}
	}
	return out
}

// describeSession prefers the generated title, falling back to the
// address so a result is always actionable for session_read.
func describeSession(i SessionBrowseInfo) string {
	label := i.Title
	if strings.TrimSpace(label) == "" {
		label = "(untitled)"
	}
	return fmt.Sprintf("%q [%s:%s]", label, i.Channel, i.ChannelID)
}

// clampArg parses an optional numeric arg, falling back to def and
// never exceeding max — the model doesn't get to widen its own limits.
func clampArg(raw string, def, max int) int {
	v := def
	if s := strings.TrimSpace(raw); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			v = n
		}
	}
	if v > max {
		v = max
	}
	return v
}

// collapseWhitespace flattens a snippet to one line so a multi-line
// match doesn't wreck the result list's readability.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// SessionToolDefs describes the session tools to the LLM.
func SessionToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "session_search",
			Path:        BuiltinScheme + "session_search",
			Description: "Search the exact text of past conversations. Use when the user refers to something specific that was said before — a command they ran, an error message, a name, a decision — and you need the actual wording rather than a general recollection. For 'what do you know about X' use memory_search instead; this finds literal text in a specific thread. Returns matching conversations with snippets; follow up with session_read to see more.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"query": {"type": "string", "description": "Literal text to find in past messages."},
					"channel": {"type": "string", "description": "Optional channel filter, e.g. \"telegram\" or \"rest\"."},
					"limit": {"type": "integer", "description": "Max conversations to return."}
				},
				"required": ["query"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "session_list",
			Path:        BuiltinScheme + "session_list",
			Description: "List stored conversations, most recently active first, with their titles and message counts. Use when the user asks what you've been talking about, or to find a thread by topic before reading it with session_read. Covers this user's own conversations plus the current one; other people's threads on this node are not listed.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"limit": {"type": "integer", "description": "Max conversations to list. Default 10."}
				},
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "session_read",
			Path:        BuiltinScheme + "session_read",
			Description: "Read a window of a stored conversation's transcript. Takes the channel and channel_id from session_search or session_list. Use from_seq to page through a long thread — each result reports the sequence numbers it covers. Prefer session_search first; reading a whole conversation is expensive in context. Addresses that didn't come from those tools will usually be refused — you can only read this user's own conversations and the current one.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"channel": {"type": "string", "description": "Channel kind, from session_search or session_list."},
					"channel_id": {"type": "string", "description": "Conversation id, from session_search or session_list."},
					"from_seq": {"type": "integer", "description": "Start at this sequence number. Omit to start at the beginning."},
					"limit": {"type": "integer", "description": "Max messages to return."}
				},
				"required": ["channel", "channel_id"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
	}
}
