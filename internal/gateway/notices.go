package gateway

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Telling the operator that something is waiting for them.
//
// propose mode fills a queue nobody is told about. The CLI can show it,
// but only to somebody who already suspects it exists — and a queue you
// have to remember to check is one you check once. Meanwhile the
// curator now expires unreviewed proposals, which turns "nobody was
// told" into "a decision was made by timeout".
//
// So the notice rides out on a turn the user is already having. That is
// the whole design decision: no push mechanism, no per-channel
// addressing, no delivery guarantees to get wrong. Any channel that can
// send a reply can carry it, which is what makes adding a channel later
// configuration rather than code.
//
// It is appended to the OUTBOUND TEXT ONLY and never enters the
// transcript. A notice recorded as an assistant message is one the
// model reads next turn and reasons about — at which point the agent is
// discussing its own pending proposals with the user, and worse, it is
// in the summary forever.

// Notice is one thing waiting for a person.
type Notice struct {
	// Text is a single line. Notices share a turn with a real reply,
	// so a paragraph is a notice that has become the message.
	Text string
}

// NoticeSource produces the notices a principal should see.
//
// Given the principal rather than deriving one, because who may be
// told is the security question here: in a group chat, everybody
// present would otherwise learn what the operator has pending.
type NoticeSource interface {
	Notices(ctx context.Context, principal string) ([]Notice, error)
}

// NoticeConfig is the operator's opt-in.
//
// Two explicit allowlists rather than a boolean. Channels decides
// WHERE — so adding Slack later is a string in a config file, not a
// code change — and Subjects decides WHO, which cannot be inferred:
// the person a notice concerns is the one who can act on it, and the
// node has no way to know that from the conversation alone.
type NoticeConfig struct {
	// Channels that may carry notices, by gateway kind ("telegram",
	// "rest"). Empty means none: silence is the safe default for a
	// feature that tells somebody about a review queue.
	Channels []string

	// Subjects that may receive them, as principals ("user:john").
	// Empty means none, for the same reason.
	Subjects []string

	// Interval is the minimum gap between notices in one conversation.
	// Zero takes the default.
	Interval time.Duration
}

// DefaultNoticeInterval is how often one conversation may be told.
//
// A day. The thing being reported changes on the scale of days, and a
// nudge appended to every turn stops being information within an hour
// — after which the operator reads past it, which is worse than never
// having sent it.
const DefaultNoticeInterval = 24 * time.Hour

// Notices appends operator notices to outbound replies.
type Notices struct {
	src NoticeSource
	cfg NoticeConfig

	// lastSent is per-process, deliberately.
	//
	// The consequence of getting it wrong is one extra line on one
	// reply, which is a different order of thing from a permission
	// surviving where it should not — so this does not earn a raft
	// round trip on the reply path. A restart or a second node may
	// produce one duplicate nudge; nobody will notice, and nothing is
	// unsafe.
	mu       sync.Mutex
	lastSent map[string]time.Time
}

// NewNotices builds the appender. A nil source disables it, which is
// what a deployment with self-learning off gets — absence rather than
// a guarded call.
func NewNotices(src NoticeSource, cfg NoticeConfig) *Notices {
	if src == nil {
		return nil
	}
	return &Notices{src: src, cfg: cfg, lastSent: map[string]time.Time{}}
}

// Append returns the reply with any notice attached.
//
// Never returns an error. A notice is a courtesy riding on somebody
// else's turn, and failing a reply the user is waiting for because the
// courtesy could not be assembled would be the wrong trade in every
// case.
// alsoKnownAs are additional identities the caller may be recognised
// by. Variadic so every existing call site is unchanged.
//
// A Telegram account with a username is attributed as "tg-@name",
// which is not a thing config can predict — an operator writes down
// the numeric id, because that is what the console shows and what
// identity resolution is keyed on. Matching only the principal meant
// a subject list naming the id could never match a turn from a user
// who had set a username, so the nudge was configured, reported
// enabled, and unable to fire.
//
// isAudience has tolerated exactly this mismatch for prompts since it
// was written. This is the same tolerance for notices.
func (n *Notices) Append(ctx context.Context, channel, conversationID, principal, reply string,
	alsoKnownAs ...string) string {
	if n == nil || !n.permitted(channel, principal, alsoKnownAs...) {
		return reply
	}
	key := channel + ":" + conversationID
	if !n.due(key) {
		return reply
	}
	notices, err := n.src.Notices(ctx, principal)
	if err != nil || len(notices) == 0 {
		return reply
	}
	// Recorded only once something is actually sent. Marking on the
	// attempt would mean an empty queue today silences a full one
	// tomorrow, and the operator would be told nothing for a day
	// because there had been nothing to tell them.
	n.markSent(key)

	var b strings.Builder
	b.WriteString(reply)
	// A separator, because the notice is not the assistant answering.
	// Without it the two run together and the nudge reads as part of
	// the reply the user asked for.
	b.WriteString("\n\n———\n")
	for i, notice := range notices {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(notice.Text)
	}
	return b.String()
}

// permitted reports whether this channel and this person are both
// opted in.
//
// Both, not either. A channel allowlist without a subject allowlist
// tells a group chat what the operator has pending; a subject
// allowlist without a channel one puts it wherever the operator
// happens to be, including channels they never configured for it.
func (n *Notices) permitted(channel, principal string, alsoKnownAs ...string) bool {
	if !contains(n.cfg.Channels, channel) {
		return false
	}
	if contains(n.cfg.Subjects, principal) {
		return true
	}
	for _, alt := range alsoKnownAs {
		if contains(n.cfg.Subjects, alt) {
			return true
		}
	}
	return false
}

func contains(list []string, want string) bool {
	if strings.TrimSpace(want) == "" {
		return false
	}
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), want) {
			return true
		}
	}
	return false
}

func (n *Notices) due(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	interval := n.cfg.Interval
	if interval <= 0 {
		interval = DefaultNoticeInterval
	}
	last, seen := n.lastSent[key]
	return !seen || time.Since(last) >= interval
}

func (n *Notices) markSent(key string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lastSent[key] = time.Now()
}

// PendingReviewNotice renders the count of things awaiting a decision.
//
// A count and a command, not a list. The operator does not need to
// evaluate three proposals inside a reply about something else — they
// need to know a queue exists and how to open it.
func PendingReviewNotice(proposals, refinements int) []Notice {
	if proposals+refinements == 0 {
		return nil
	}
	var parts []string
	if proposals > 0 {
		parts = append(parts, fmt.Sprintf("%s waiting for approval", plural(proposals, "skill")))
	}
	if refinements > 0 {
		parts = append(parts, fmt.Sprintf("%s waiting for review", plural(refinements, "refinement")))
	}
	return []Notice{{Text: fmt.Sprintf(
		"%s — `lobslaw learned pending --all`", strings.Join(parts, ", "))}}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// noticeSubject derives the principal a notice is addressed to.
//
// The same "user:<id>" form the approval subject uses, so an operator
// who put their principal in one allowlist has put it in a shape the
// other recognises. Deriving it two different ways would make
// notices silently miss the person who can act on them.
func noticeSubject(claims *types.Claims) string {
	if claims == nil || strings.TrimSpace(claims.UserID) == "" {
		return ""
	}
	return "user:" + claims.UserID
}
